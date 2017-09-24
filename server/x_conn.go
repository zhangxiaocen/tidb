// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"io"
	"net"

	log "github.com/Sirupsen/logrus"
	"github.com/juju/errors"
	"github.com/pingcap/tidb/driver"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/arena"
	"github.com/pingcap/tidb/xprotocol/capability"
	xutil "github.com/pingcap/tidb/xprotocol/util"
	"github.com/pingcap/tidb/xprotocol/xpacketio"
	"github.com/pingcap/tipb/go-mysqlx"
	"github.com/pingcap/tipb/go-mysqlx/Connection"
)

// mysqlXClientConn represents a connection between server and client,
// it maintains connection specific state, handles client query.
type mysqlXClientConn struct {
	pkt          *xpacketio.XPacketIO // a helper to read and write data in packet format.
	conn         net.Conn
	xauth        *XAuth
	xsession     *xSession
	server       *Server                        // a reference of server instance.
	capability   uint32                         // client capability affects the way server handles client request.
	capabilities Mysqlx_Connection.Capabilities // mysql shell client capabilities affects the way server handles client request.
	connectionID uint32                         // atomically allocated by a global variable, unique in process scope.
	collation    uint8                          // collation used by client, may be different from the collation used by database.
	user         string                         // user of the client.
	dbname       string                         // default database name.
	salt         []byte                         // random bytes used for authentication.
	alloc        arena.Allocator                // an memory allocator for reducing memory allocation.
	lastCmd      string                         // latest sql query string, currently used for logging error.
	ctx          driver.QueryCtx                // an interface to execute sql statements.
	attrs        map[string]string              // attributes parsed from client handshake response, not used for now.
	killed       bool
}

func (xcc *mysqlXClientConn) Run() {
	defer func() {
		recover()
		xcc.Close()
		log.Infof("[%d] connection exit.", xcc.connectionID)
	}()

	log.Infof("[%d] establish connection successfully.")
	for !xcc.killed {
		tp, payload, err := xcc.pkt.ReadPacket()
		if err != nil {
			if terror.ErrorNotEqual(err, io.EOF) {
				log.Errorf("[%d] read packet error, close this connection %s",
					xcc.connectionID, errors.ErrorStack(err))
			}
			return
		}
		log.Debugf("[%d] receive msg type[%d]", xcc.connectionID, tp)
		if err = xcc.dispatch(tp, payload); err != nil {
			if terror.ErrorEqual(err, terror.ErrResultUndetermined) {
				log.Errorf("[%d] result undetermined error, close this connection %s",
					xcc.connectionID, errors.ErrorStack(err))
				return
			} else if terror.ErrorEqual(err, terror.ErrCritical) {
				log.Errorf("[%d] critical error, stop the server listener %s",
					xcc.connectionID, errors.ErrorStack(err))
				select {
				case xcc.server.stopListenerCh <- struct{}{}:
				default:
				}
				return
			}
			log.Warnf("[%d] dispatch error: %s", xcc.connectionID, err)
			xcc.writeError(err)
		}
	}
}

func (xcc *mysqlXClientConn) Close() error {
	xcc.server.rwlock.Lock()
	delete(xcc.server.clients, xcc.connectionID)
	connections := len(xcc.server.clients)
	xcc.server.rwlock.Unlock()
	connGauge.Set(float64(connections))
	xcc.conn.Close()
	if xcc.ctx != nil {
		return xcc.ctx.Close()
	}
	return nil
}

func (xcc *mysqlXClientConn) handshakeConnection() error {
	tp, msg, err := xcc.pkt.ReadPacket()
	if err != nil {
		return errors.Trace(err)
	}
	xcc.configCapabilities()
	caps, err := capability.CheckCapabilitiesPrepareSetMsg(tp, msg)
	if err != nil {
		return errors.Trace(err)
	}
	for _, v := range caps {
		xcc.addCapability(v)
	}
	if err = xcc.pkt.WritePacket(Mysqlx.ServerMessages_OK, []byte{}); err != nil {
		return errors.Trace(err)
	}
	tp, msg, err = xcc.pkt.ReadPacket()
	if err != nil {
		return errors.Trace(err)
	}
	if err = capability.CheckCapabilitiesGetMsg(tp, msg); err != nil {
		return errors.Trace(err)
	}

	resp, err := xcc.capabilities.Marshal()
	if err != nil {
		return errors.Trace(err)
	}
	if err = xcc.pkt.WritePacket(Mysqlx.ServerMessages_CONN_CAPABILITIES, resp); err != nil {
		return errors.Trace(err)
	}
	tp, msg, err = xcc.pkt.ReadPacket()
	if err != nil {
		return errors.Trace(err)
	}
	if err = capability.CheckCapabilitiesSetMsg(tp, msg); err != nil {
		return errors.Trace(err)
	}
	return xcc.writeError(xutil.ErXCapabilitiesPrepareFailed.GenByArgs("tls"))
}


func (xcc *mysqlXClientConn) auth() error {
	for {
		tp, msg, err := xcc.pkt.ReadPacket()
		if err != nil {
			return err
		}

		if err = xcc.xauth.handleMessage(tp, msg); err != nil {
			log.Errorf("[%d] auth failed on x-protocol, get error %s", xcc.connectionID, err.Error())
			xcc.writeError(err)
			return err
		}

		if xcc.xauth.ready() {
			break
		}
	}
	return nil
}

func (xcc *mysqlXClientConn) handshake() error {
	if err := xcc.handshakeConnection(); err != nil {
		return err
	}

	// Open session
	ctx, err := xcc.server.driver.OpenCtx(uint64(xcc.connectionID), xcc.capability, uint8(xcc.collation), xcc.dbname, nil)
	if err != nil {
		return errors.Trace(err)
	}
	xcc.ctx = ctx
	xcc.xsession = xcc.createXSession()
	xcc.xauth = xcc.createAuth(xcc.connectionID)

	// do auth
	if err := xcc.auth(); err != nil {
		return err
	}

	if xcc.dbname != "" {
		if err := xcc.useDB(xcc.dbname); err != nil {
			return errors.Trace(err)
		}
	}
	xcc.ctx.SetSessionManager(xcc.server)

	return nil
}

func (xcc *mysqlXClientConn) dispatch(tp Mysqlx.ClientMessages_Type, payload []byte) error {
	msgType := Mysqlx.ClientMessages_Type(tp)
	switch msgType {
	case Mysqlx.ClientMessages_SESS_CLOSE, Mysqlx.ClientMessages_SESS_RESET, Mysqlx.ClientMessages_CON_CLOSE:
		return xcc.xauth.handleMessage(msgType, payload)
	default:
		return xcc.xsession.handleMessage(msgType, payload)
	}

	return nil
}

func (xcc *mysqlXClientConn) flush() error {
	return xcc.pkt.Flush()
}

func (xcc *mysqlXClientConn) writeError(e error) error {
	var (
		m  *mysql.SQLError
		te *terror.Error
		ok bool
	)
	originErr := errors.Cause(e)
	if te, ok = originErr.(*terror.Error); ok {
		m = te.ToSQLError()
	} else {
		m = mysql.NewErrf(mysql.ErrUnknown, "%s", e.Error())
	}
	errMsg, err := xutil.XErrorMessage(m.Code, m.Message, m.State).Marshal()
	if err != nil {
		return err
	}
	return xcc.pkt.WritePacket(Mysqlx.ServerMessages_ERROR, errMsg)
}

func (xcc *mysqlXClientConn) isKilled() bool {
	return xcc.killed
}

func (xcc *mysqlXClientConn) Cancel(query bool) {
	xcc.ctx.Cancel()
	if !query {
		xcc.killed = true
	}
}

func (xcc *mysqlXClientConn) id() uint32 {
	return xcc.connectionID
}

func (xcc *mysqlXClientConn) showProcess() util.ProcessInfo {
	//return xcc.ctx.ShowProcess()
	return util.ProcessInfo{}
}

func (xcc *mysqlXClientConn) useDB(db string) (err error) {
	// if input is "use `SELECT`", mysql client just send "SELECT"
	// so we add `` around db.
	_, err = xcc.ctx.Execute("use `" + db + "`")
	if err != nil {
		return errors.Trace(err)
	}
	xcc.dbname = db
	return
}

func (xcc *mysqlXClientConn) createAuth(id uint32) *XAuth {
	return &XAuth{
		xcc:               xcc,
		mState:            authenticating,
		mStateBeforeClose: authenticating,
	}
}

func (xcc *mysqlXClientConn) addCapability(h capability.Handler) {
	xcc.capabilities.Capabilities = append(xcc.capabilities.Capabilities, h.Get())
}

func (xcc *mysqlXClientConn) configCapabilities() {
	xcc.addCapability(&capability.HandlerAuthMechanisms{Values: []string{"MYSQL41"}})
	xcc.addCapability(&capability.HandlerReadOnlyValue{Name: "doc.formats", Value: "text"})
	xcc.addCapability(&capability.HandlerReadOnlyValue{Name: "node_type", Value: "mysql"})
}

type xSession struct {
	xsql  *xSQL
}

func (xcc *mysqlXClientConn) createXSession() *xSession {
	return &xSession{
		xsql: createContext(xcc),
	}
}

func (xs *xSession) handleMessage(msgType Mysqlx.ClientMessages_Type, payload []byte) error {
	switch msgType {
	case Mysqlx.ClientMessages_SQL_STMT_EXECUTE:
		return xs.xsql.dealSQLStmtExecute(payload)
	// @TODO will support in next pr
	case Mysqlx.ClientMessages_CRUD_FIND, Mysqlx.ClientMessages_CRUD_INSERT, Mysqlx.ClientMessages_CRUD_UPDATE, Mysqlx.ClientMessages_CRUD_DELETE,
		Mysqlx.ClientMessages_CRUD_CREATE_VIEW, Mysqlx.ClientMessages_CRUD_MODIFY_VIEW, Mysqlx.ClientMessages_CRUD_DROP_VIEW:
		return xutil.ErXBadMessage
	// @TODO will support in next pr
	case Mysqlx.ClientMessages_EXPECT_OPEN, Mysqlx.ClientMessages_EXPECT_CLOSE:
		return xutil.ErXBadMessage
	default:
		return xutil.ErXBadMessage
	}
}
