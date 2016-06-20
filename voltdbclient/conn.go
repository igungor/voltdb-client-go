/* This file is part of VoltDB.
 * Copyright (C) 2008-2016 VoltDB Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with VoltDB.  If not, see <http://www.gnu.org/licenses/>.
 */

package voltdbclient

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
)

var qHandle int64 = 0 // each query has a unique handle.

// connectionData are the values returned by a successful login.
type connectionData struct {
	hostId      int32
	connId      int64
	leaderAddr  int32
	buildString string
}

type VoltConn struct {
	reader      io.Reader
	writer      io.Writer
	connData    *connectionData
	execs       map[int64]*VoltExecResult
	queries     map[int64]*VoltQueryResult
	netListener *NetworkListener
	nlwg        sync.WaitGroup
	isOpen      bool
}

func newVoltConn(reader io.Reader, writer io.Writer, connData *connectionData) *VoltConn {
	var vc = new(VoltConn)
	vc.reader = reader
	vc.writer = writer
	vc.execs = make(map[int64]*VoltExecResult)
	vc.queries = make(map[int64]*VoltQueryResult)
	vc.nlwg = sync.WaitGroup{}
	vc.netListener = newListener(reader, vc.nlwg)
	vc.netListener.start()
	vc.isOpen = true
	return vc
}

func (vc VoltConn) Begin() (driver.Tx, error) {
	return nil, errors.New("VoltDB does not support transactions, VoltDB autocommits")
}

func (vc VoltConn) Close() (err error) {
	// stop the network listener, wait for it to stop.
	vc.netListener.stop()
	vc.nlwg.Wait()
	if vc.reader != nil {
		tcpConn := vc.reader.(*net.TCPConn)
		err = tcpConn.Close()
	}
	vc.reader = nil
	vc.writer = nil
	vc.connData = nil
	vc.isOpen = false
	return err
}

func OpenConn(connInfo string) (*VoltConn, error) {
	// for now, at least, connInfo is host and port.
	raddr, err := net.ResolveTCPAddr("tcp", connInfo)
	if err != nil {
		return nil, fmt.Errorf("Error resolving %v.", connInfo)
	}
	var tcpConn *net.TCPConn
	if tcpConn, err = net.DialTCP("tcp", nil, raddr); err != nil {
		return nil, err
	}
	login, err := serializeLoginMessage("", "")
	if err != nil {
		return nil, err
	}
	writeLoginMessage(tcpConn, &login)
	connData, err := readLoginResponse(tcpConn)
	if err != nil {
		return nil, err
	}
	return newVoltConn(tcpConn, tcpConn, connData), nil
}

func (vc VoltConn) Prepare(query string) (driver.Stmt, error) {
	panic("Prepare is not supported by a Volt Connection")
}

func (vc VoltConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if !vc.isOpen {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.netListener.registerExec(handle)
	if err := vc.serializeQuery(vc.writer, query, handle, args); err != nil {
		vc.netListener.removeExec(handle)
		return VoltResult{}, err
	}
	return <-c, nil
}

func (vc VoltConn) ExecAsync(query string, args []driver.Value) (*VoltExecResult, error) {
	if !vc.isOpen {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.netListener.registerExec(handle)
	ver := newVoltExecResult(&vc, handle, c)
	vc.registerExec(handle, ver)
	if err := vc.serializeQuery(vc.writer, query, handle, args); err != nil {
		vc.netListener.removeExec(handle)
		return nil, err
	}
	return ver, nil
}

func (vc VoltConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if !vc.isOpen {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.netListener.registerQuery(handle)
	if err := vc.serializeQuery(vc.writer, query, handle, args); err != nil {
		vc.netListener.removeQuery(handle)
		return VoltRows{}, err
	}
	return <-c, nil
}

func (vc VoltConn) QueryAsync(query string, args []driver.Value) (*VoltQueryResult, error) {
	if !vc.isOpen {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.netListener.registerQuery(handle)
	vqr := newVoltQueryResult(&vc, handle, c)
	vc.registerQuery(handle, vqr)
	if err := vc.serializeQuery(vc.writer, query, handle, args); err != nil {
		vc.netListener.removeQuery(handle)
		return nil, err
	}
	return vqr, nil
}

func (vc VoltConn) Drain(vqrs []*VoltQueryResult) {
	idxs := []int{} // index into the given slice
	cases := []reflect.SelectCase{}
	for idx, vqr := range vqrs {
		if vqr.isActive() {
			idxs = append(idxs, idx)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(vqr.channel())})
		}
	}

	for len(idxs) > 0 {
		chosen, val, ok := reflect.Select(cases)

		// idiom for removing from the middle of a slice
		idx := idxs[chosen]
		idxs[chosen] = idxs[len(idxs)-1]
		idxs = idxs[:len(idxs)-1]

		cases[chosen] = cases[len(cases)-1]
		cases = cases[:len(cases)-1]

		chosenQuery := vqrs[idx]
		// if not ok, the channel was closed
		if !ok {
			chosenQuery.setError(errors.New("Result was not available, channel was closed"))
		} else {
			// check the returned value
			if val.Kind() != reflect.Interface {
				chosenQuery.setError(errors.New("unexpected return type, not an interface"))
			} else {
				rows, ok := val.Interface().(driver.Rows)
				if !ok {
					chosenQuery.setError(errors.New("unexpected return type, not driver.Rows"))
				} else {
					vrows := rows.(VoltRows)
					if vrows.error() != nil {
						chosenQuery.setError(vrows.error())
					} else {
						chosenQuery.setRows(rows)
					}
				}
			}
		}
	}
}

func (vc VoltConn) DrainAll() []*VoltQueryResult {
	result := make([]*VoltQueryResult, len(vc.queries))
	i := 0
	for _, vcr := range vc.queries {
		result[i] = vcr
		i++
	}
	vc.Drain(result)
	return result
}

func (vc VoltConn) ExecutingQueries() []*VoltQueryResult {
	// don't copy the queries themselves, but copy the list
	eqs := make([]*VoltQueryResult, len(vc.queries))
	i := 0
	for _, eq := range vc.queries {
		eqs[i] = eq
		i++
	}
	return eqs
}

func (vc VoltConn) registerExec(handle int64, ver *VoltExecResult) {
	vc.execs[handle] = ver
}

func (vc VoltConn) registerQuery(handle int64, vcr *VoltQueryResult) {
	vc.queries[handle] = vcr
}

func (vc VoltConn) removeExec(han int64) {
	delete(vc.execs, han)
}

func (vc VoltConn) removeQuery(han int64) {
	delete(vc.queries, han)
}

func writeLoginMessage(writer io.Writer, buf *bytes.Buffer) {
	// length includes protocol version.
	length := buf.Len() + 2
	var netmsg bytes.Buffer
	writeInt(&netmsg, int32(length))
	writeProtoVersion(&netmsg)
	writePasswordHashVersion(&netmsg)
	// 1 copy + 1 n/w write benchmarks faster than 2 n/w writes.
	io.Copy(&netmsg, buf)
	io.Copy(writer, &netmsg)
}

func readLoginResponse(reader io.Reader) (*connectionData, error) {
	buf, err := readMessage(reader)
	if err != nil {
		return nil, err
	}
	connData, err := deserializeLoginResponse(buf)
	return connData, err
}

type VoltQueryResult struct {
	conn   *VoltConn
	han    int64
	ch     <-chan driver.Rows
	rows   driver.Rows
	err    error
	active bool
}

func newVoltQueryResult(conn *VoltConn, han int64, ch <-chan driver.Rows) *VoltQueryResult {
	var vqr = new(VoltQueryResult)
	vqr.conn = conn
	vqr.han = han
	vqr.ch = ch
	vqr.active = true
	return vqr
}

func (vqr *VoltQueryResult) Rows() (driver.Rows, error) {
	if !vqr.active {
		if vqr.err != nil {
			return nil, vqr.err
		}
		return vqr.rows, nil
	} else {
		rows := <-vqr.ch
		vrows := rows.(VoltRows)
		if err := vrows.error(); err != nil {
			vqr.setError(err)
			return nil, err
		}
		vqr.setRows(rows)
		return vqr.rows, nil
	}
}

func (vqr *VoltQueryResult) channel() <-chan driver.Rows {
	return vqr.ch
}

func (vqr *VoltQueryResult) handle() int64 {
	return vqr.han
}

func (vqr *VoltQueryResult) isActive() bool {
	return vqr.active
}

func (vqr *VoltQueryResult) setError(err error) {
	vqr.err = err
	vqr.active = false
	vqr.conn.removeQuery(vqr.han)
}

func (vqr *VoltQueryResult) setRows(rows driver.Rows) {
	vqr.rows = rows
	vqr.active = false
	vqr.conn.removeQuery(vqr.han)
}

type VoltExecResult struct {
	conn   *VoltConn
	han    int64
	ch     <-chan driver.Result
	result driver.Result
	err    error
	active bool
}

func newVoltExecResult(conn *VoltConn, han int64, ch <-chan driver.Result) *VoltExecResult {
	var ver = new(VoltExecResult)
	ver.conn = conn
	ver.han = han
	ver.ch = ch
	ver.active = true
	return ver
}

func (ver *VoltExecResult) Result() (driver.Result, error) {
	if !ver.active {
		return ver.result, ver.err
	} else {
		result := <-ver.ch
		ver.setResult(result)
		return ver.result, nil
	}
}

func (ver *VoltExecResult) channel() <-chan driver.Result {
	return ver.ch
}

func (ver *VoltExecResult) handle() int64 {
	return ver.han
}

func (ver *VoltExecResult) isActive() bool {
	return ver.active
}

func (ver *VoltExecResult) setError(err error) {
	ver.err = err
	ver.conn.removeExec(ver.han)
	ver.active = false
}

func (ver *VoltExecResult) setResult(result driver.Result) {
	if !ver.active {
		panic("Tried to set result on inactive exec result")
	}
	ver.result = result
	ver.conn.removeExec(ver.han)
	ver.active = false
}

func (vc VoltConn) serializeQuery(writer io.Writer, procedure string, handle int64, args []driver.Value) error {

	var call bytes.Buffer
	var err error

	// Serialize the procedure call and its params.
	// Use 0 for handle; it's not necessary in pure sync client.
	if call, err = serializeStatement(procedure, handle, args); err != nil {
		return err
	}

	var netmsg bytes.Buffer
	writeInt(&netmsg, int32(call.Len()))
	io.Copy(&netmsg, &call)
	io.Copy(writer, &netmsg)
	return nil
}

// Null Value type
type NullValue struct {
	colType int8
}

func NewNullValue(colType int8) *NullValue {
	var nv = new(NullValue)
	nv.colType = colType
	return nv
}

func (nv *NullValue) ColType() int8 {
	return nv.colType
}
