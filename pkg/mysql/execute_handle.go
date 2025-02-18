/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mysql

import (
	"strings"
)

import (
	"github.com/arana-db/parser"
)

import (
	"github.com/arana-db/arana/pkg/constants/mysql"
	"github.com/arana-db/arana/pkg/mysql/errors"
	"github.com/arana-db/arana/pkg/proto"
	"github.com/arana-db/arana/pkg/proto/hint"
	"github.com/arana-db/arana/pkg/security"
	"github.com/arana-db/arana/pkg/trace"
	"github.com/arana-db/arana/pkg/util/log"
)

func (l *Listener) handleInitDB(c *Conn, ctx *proto.Context) error {
	db := string(ctx.Data[1:])
	c.recycleReadPacket()

	var allow bool
	for _, it := range security.DefaultTenantManager().GetClusters(c.Tenant) {
		if db == it {
			allow = true
			break
		}
	}

	if !allow {
		if err := c.writeErrorPacketFromError(errors.NewSQLError(mysql.ERBadDb, "", "Unknown database '%s'", db)); err != nil {
			log.Errorf("failed to write ComInitDB error to %s: %v", c, err)
			return err
		}
		return nil
	}

	c.Schema = db
	err := l.executor.ExecuteUseDB(ctx)
	if err != nil {
		return err
	}
	if err = c.writeOKPacket(0, 0, c.StatusFlags, 0); err != nil {
		log.Errorf("Error writing ComInitDB result to %s: %v", c, err)
		return err
	}

	return nil
}

func (l *Listener) handleQuery(c *Conn, ctx *proto.Context) error {
	c.recycleReadPacket()

	handleOnce := func(result proto.Result, failure error, warn uint16, hasMore bool) error {
		c.startWriterBuffering()
		defer func() {
			if err := c.endWriterBuffering(); err != nil {
				log.Errorf("conn %v: flush() failed: %v", ctx.ConnectionID, err)
			}
		}()

		if failure != nil {
			log.Errorf("executor com_query error %v: %+v", ctx.ConnectionID, failure)
			if err := c.writeErrorPacketFromError(failure); err != nil {
				log.Errorf("Error writing query error to client %v: %v", ctx.ConnectionID, err)
				return err
			}
			return nil
		}

		if result == nil {
			log.Errorf("executor com_query error %v: %+v", ctx.ConnectionID, "un dataset")
			if err := c.writeErrorPacketFromError(errors.NewSQLError(mysql.ERBadNullError, mysql.SSUnknownSQLState, "un dataset")); err != nil {
				log.Errorf("Error writing query error to client %v: %v", ctx.ConnectionID, failure)
				return err
			}
			return nil
		}

		var ds proto.Dataset
		if ds, failure = result.Dataset(); failure != nil {
			log.Errorf("get dataset error %v: %v", ctx.ConnectionID, failure)
			if err := c.writeErrorPacketFromError(failure); err != nil {
				log.Errorf("Error writing query error to client %v: %v", ctx.ConnectionID, err)
				return err
			}
			return nil
		}

		if ds == nil {
			// A successful callback with no fields means that this was a
			// DML or other write-only operation.
			//
			// We should not send any more packets after this, but make sure
			// to extract the affected rows and last insert id from the result
			// struct here since clients expect it.
			var (
				affected, _ = result.RowsAffected()
				insertId, _ = result.LastInsertId()
			)

			statusFlag := c.StatusFlags
			if hasMore {
				statusFlag |= mysql.ServerMoreResultsExists
			}

			if err := c.writeOKPacket(affected, insertId, statusFlag, warn); err != nil {
				log.Errorf("failed to write OK packet into client %v: %v", ctx.ConnectionID, err)
				return err
			}
			return nil
		}

		fields, _ := ds.Fields()

		if err := c.writeFields(fields); err != nil {
			log.Errorf("write fields error %v: %v", ctx.ConnectionID, err)
			return err
		}
		if err := c.writeDataset(ds); err != nil {
			log.Errorf("write dataset error %v: %v", ctx.ConnectionID, err)
			return err
		}
		if err := c.writeEndResult(hasMore, 0, 0, warn); err != nil {
			log.Errorf("Error writing result to %s: %v", c, err)
			return err
		}
		return nil
	}
	type compositeResult struct {
		r proto.Result
		w uint16
		e error
	}

	var prev *compositeResult
	err := l.executor.ExecutorComQuery(ctx, func(result proto.Result, warns uint16, failure error) error {
		if prev != nil {
			if err := handleOnce(prev.r, prev.e, prev.w, true); err != nil {
				return err
			}
		}
		prev = &compositeResult{
			r: result,
			w: warns,
			e: failure,
		}
		return nil
	})
	if err != nil {
		return err
	}

	if prev != nil {
		if err := handleOnce(prev.r, prev.e, prev.w, false); err != nil {
			return err
		}
	}

	return nil
}

func (l *Listener) handleFieldList(c *Conn, ctx *proto.Context) error {
	c.recycleReadPacket()
	fields, err := l.executor.ExecuteFieldList(ctx)
	if err != nil {
		log.Errorf("Conn %v: Error write field list: %v", c, err)
		if wErr := c.writeErrorPacketFromError(err); wErr != nil {
			// If we can't even write the error, we're done.
			log.Errorf("Conn %v: Error write field list error: %v", c, wErr)
			return wErr
		}
	}

	// Combine the fields into a package to send
	var des []byte
	for _, field := range fields {
		fld := field.(*Field)
		des = append(des, c.DefColumnDefinition(fld)...)
	}

	des = append(des, c.buildEOFPacket(0, 2)...)

	if err = c.writePacketForFieldList(des); err != nil {
		return err
	}

	return nil
}

func (l *Listener) handleStmtExecute(c *Conn, ctx *proto.Context) error {
	c.startWriterBuffering()
	defer func() {
		if err := c.endWriterBuffering(); err != nil {
			log.Errorf("conn %v: flush() failed: %v", ctx.ConnectionID, err)
		}
	}()

	var (
		stmtID uint32
		err    error
	)

	stmtID, _, err = c.parseComStmtExecute(&l.stmts, ctx.Data)
	c.recycleReadPacket()

	if stmtID != uint32(0) {
		defer func() {
			// Allocate a new bindvar map every time since VTGate.Execute() mutates it.
			if prepare, ok := l.stmts.Load(stmtID); ok {
				prepareStmt, _ := prepare.(*proto.Stmt)
				prepareStmt.BindVars = make(map[string]proto.Value, prepareStmt.ParamsCount)
			}
		}()
	}

	if err != nil {
		if wErr := c.writeErrorPacketFromError(err); wErr != nil {
			// If we can't even write the error, we're done.
			log.Error("Error writing query error to client %v: %v", ctx.ConnectionID, wErr)
			return wErr
		}
		return nil
	}

	prepareStmt, _ := l.stmts.Load(stmtID)
	ctx.Stmt = prepareStmt.(*proto.Stmt)

	var (
		result proto.Result
		warn   uint16
	)

	if result, warn, err = l.executor.ExecutorComStmtExecute(ctx); err != nil {
		if wErr := c.writeErrorPacketFromError(err); wErr != nil {
			log.Errorf("Error writing query error to client %v: %v, executor error: %v", ctx.ConnectionID, wErr, err)
			return wErr
		}
		return nil
	}

	var ds proto.Dataset
	if ds, err = result.Dataset(); err != nil {
		if wErr := c.writeErrorPacketFromError(err); wErr != nil {
			log.Errorf("Error writing query error to client %v: %v, executor error: %v", ctx.ConnectionID, wErr, err)
			return wErr
		}
		return nil
	}

	if ds == nil {
		// A successful callback with no fields means that this was a
		// DML or other write-only operation.
		//
		// We should not send any more packets after this, but make sure
		// to extract the affected rows and last insert id from the result
		// struct here since clients expect it.
		affected, _ := result.RowsAffected()
		lastInsertId, _ := result.LastInsertId()
		return c.writeOKPacket(affected, lastInsertId, c.StatusFlags, warn)
	}

	defer func() {
		_ = ds.Close()
	}()

	fields, _ := ds.Fields()

	if err = c.writeFields(fields); err != nil {
		return err
	}
	if err = c.writeDatasetBinary(ds); err != nil {
		return err
	}
	if err = c.writeEndResult(false, 0, 0, warn); err != nil {
		log.Errorf("Error writing result to %s: %v", c, err)
		return err
	}
	return nil
}

func (l *Listener) handlePrepare(c *Conn, ctx *proto.Context) error {
	query := string(ctx.Data[1:])
	c.recycleReadPacket()

	// Populate PrepareData
	statementID := l.statementID.Inc()

	stmt := &proto.Stmt{
		StatementID: statementID,
		PrepareStmt: query,
	}
	p := parser.New()
	act, err := p.ParseOneStmt(stmt.PrepareStmt, "", "")
	if err != nil {
		log.Errorf("Conn %v: Error parsing prepared statement: %v", c, err)
		if wErr := c.writeErrorPacketFromError(err); wErr != nil {
			log.Errorf("Conn %v: Error writing prepared statement error: %v", c, wErr)
			return wErr
		}
	}

	for _, it := range act.Hints() {
		var h *hint.Hint
		if h, err = hint.Parse(it); err != nil {
			if wErr := c.writeErrorPacketFromError(err); wErr != nil {
				log.Errorf("Conn %v: Error writing prepared statement error: %v", c, wErr)
				return wErr
			}
		}
		stmt.Hints = append(stmt.Hints, h)
	}

	stmt.StmtNode = act

	paramsCount := uint16(strings.Count(query, "?"))

	if paramsCount > 0 {
		stmt.ParamsCount = paramsCount
		stmt.ParamsType = make([]int32, paramsCount)
		stmt.BindVars = make(map[string]proto.Value, paramsCount)
	}

	l.stmts.Store(statementID, stmt)

	trace.Extract(ctx, stmt.Hints)

	return c.writePrepare(stmt)
}

func (l *Listener) handleStmtReset(c *Conn, ctx *proto.Context) error {
	stmtID, _, ok := readUint32(ctx.Data, 1)
	c.recycleReadPacket()
	if ok {
		if prepare, ok := l.stmts.Load(stmtID); ok {
			prepareStmt, _ := prepare.(*proto.Stmt)
			prepareStmt.BindVars = make(map[string]proto.Value)
		}
	}
	return c.writeOKPacket(0, 0, c.StatusFlags, 0)
}

func (l *Listener) handleSetOption(c *Conn, ctx *proto.Context) error {
	operation, _, ok := readUint16(ctx.Data, 1)
	c.recycleReadPacket()
	if ok {
		switch operation {
		case 0:
			c.Capabilities |= mysql.CapabilityClientMultiStatements
		case 1:
			c.Capabilities &^= mysql.CapabilityClientMultiStatements
		default:
			log.Errorf("Got unhandled packet (ComSetOption default) from client %v, returning error: %v", ctx.ConnectionID, ctx.Data)
			if err := c.writeErrorPacket(mysql.ERUnknownComError, mysql.SSUnknownComError, "error handling packet: %v", ctx.Data); err != nil {
				log.Errorf("Error writing error packet to client: %v", err)
				return err
			}
		}
		if err := c.writeEndResult(false, 0, 0, 0); err != nil {
			log.Errorf("Error writeEndResult error %v ", err)
			return err
		}
	}
	log.Errorf("Got unhandled packet (ComSetOption else) from client %v, returning error: %v", ctx.ConnectionID, ctx.Data)
	if err := c.writeErrorPacket(mysql.ERUnknownComError, mysql.SSUnknownComError, "error handling packet: %v", ctx.Data); err != nil {
		log.Errorf("Error writing error packet to client: %v", err)
		return err
	}
	return nil
}
