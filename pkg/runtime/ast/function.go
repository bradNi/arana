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

package ast

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

import (
	"github.com/pkg/errors"
)

const (
	_ FunctionArgType = iota
	FunctionArgConstant
	FunctionArgColumn
	FunctionArgExpression
	FunctionArgFunction
	FunctionArgAggrFunction
	FunctionArgCaseWhenElseFunction
	FunctionArgCastFunction
)

const (
	_ FunctionType = iota
	Fudf
	Fscalar
	Fspec
	Fpasswd
)

type (
	FunctionArgType uint8
	FunctionType    uint8
)

func (f FunctionType) String() string {
	switch f {
	case Fudf:
		return "UDF"
	case Fscalar:
		return "SCALAR"
	case Fspec:
		return "SPEC"
	case Fpasswd:
		return "PASSWORD"
	default:
		panic("unreachable")
	}
}

var (
	_ Restorer = (*FunctionArg)(nil)
	_ Restorer = (*Function)(nil)
	_ Restorer = (*AggrFunction)(nil)
	_ Restorer = (*CaseWhenElseFunction)(nil)
	_ Restorer = (*CastFunction)(nil)
)

type Function struct {
	typ  FunctionType
	name string
	args []*FunctionArg
}

func (f *Function) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitFunction(f)
}

func (f *Function) Type() FunctionType {
	return f.typ
}

func (f *Function) Name() string {
	switch f.typ {
	case Fspec, Fscalar, Fpasswd:
		return strings.ToUpper(f.name)
	default:
		return f.name
	}
}

func (f *Function) Args() []*FunctionArg {
	return f.args
}

func (f *Function) Restore(flag RestoreFlag, sb *strings.Builder, args *[]int) error {
	sb.WriteString(f.Name())
	sb.WriteByte('(')

	if len(f.args) > 0 {
		if err := f.args[0].Restore(flag, sb, args); err != nil {
			return errors.WithStack(err)
		}
		for i := 1; i < len(f.args); i++ {
			sb.WriteByte(',')
			if err := f.args[i].Restore(flag, sb, args); err != nil {
				return errors.WithStack(err)
			}
		}
	}

	sb.WriteByte(')')
	return nil
}

func (f *Function) CntParams() int {
	var n int
	for _, it := range f.args {
		n += it.CntParams()
	}
	return n
}

type FunctionArg struct {
	Type  FunctionArgType
	Value interface{}
}

func (f *FunctionArg) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitFunctionArg(f)
}

func (f *FunctionArg) Restore(flag RestoreFlag, sb *strings.Builder, args *[]int) error {
	var err error
	switch f.Type {
	case FunctionArgColumn:
		err = f.Value.(ColumnNameExpressionAtom).Restore(flag, sb, args)
	case FunctionArgExpression:
		err = f.Value.(ExpressionNode).Restore(flag, sb, args)
	case FunctionArgConstant:
		sb.WriteString(constant2string(f.Value))
	case FunctionArgFunction:
		err = f.Value.(*Function).Restore(flag, sb, args)
	case FunctionArgAggrFunction:
		err = f.Value.(*AggrFunction).Restore(flag, sb, args)
	case FunctionArgCaseWhenElseFunction:
		err = f.Value.(*CaseWhenElseFunction).Restore(flag, sb, args)
	case FunctionArgCastFunction:
		err = f.Value.(*CastFunction).Restore(flag, sb, args)
	default:
		panic("unreachable")
	}

	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (f *FunctionArg) CntParams() int {
	c, ok := f.Value.(paramsCounter)
	if ok {
		return c.CntParams()
	}
	return 0
}

const (
	_flagAggrCountStar AggrFunctionFlag = 1 << iota
)

const (
	AggrAvg   = "AVG"
	AggrMax   = "MAX"
	AggrMin   = "MIN"
	AggrSum   = "SUM"
	AggrCount = "COUNT"
)

const (
	Distinct = "DISTINCT"
	All      = "ALL"
)

type AggrFunctionFlag uint8

type AggrFunction struct {
	flag       AggrFunctionFlag
	name       string
	aggregator string
	args       []*FunctionArg
}

func (af *AggrFunction) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitFunctionAggregate(af)
}

func (af *AggrFunction) Restore(flag RestoreFlag, sb *strings.Builder, args *[]int) error {
	sb.WriteString(af.name)
	sb.WriteByte('(')
	if af.IsCountStar() {
		sb.WriteByte('*')
		sb.WriteByte(')')
		return nil
	}

	if len(af.aggregator) > 0 {
		sb.WriteString(af.aggregator)
		sb.WriteByte(' ')
	}

	if len(af.args) < 1 {
		sb.WriteByte(')')
		return nil
	}

	if err := af.args[0].Restore(flag, sb, args); err != nil {
		return errors.WithStack(err)
	}

	for i := 1; i < len(af.args); i++ {
		sb.WriteString(", ")
		if err := af.args[i].Restore(flag, sb, args); err != nil {
			return errors.WithStack(err)
		}
	}

	sb.WriteByte(')')
	return nil
}

func (af *AggrFunction) Aggregator() (string, bool) {
	if len(af.aggregator) < 1 {
		return "", false
	}
	return af.aggregator, true
}

func (af *AggrFunction) Name() string {
	return af.name
}

func (af *AggrFunction) Args() []*FunctionArg {
	return af.args
}

func (af *AggrFunction) IsCountStar() bool {
	return af.flag&_flagAggrCountStar != 0
}

func (af *AggrFunction) EnableCountStar() {
	af.flag |= _flagAggrCountStar
}

func NewAggrFunction(name string, aggregator string, args []*FunctionArg) *AggrFunction {
	return &AggrFunction{
		name:       name,
		aggregator: aggregator,
		args:       args,
	}
}

type CaseWhenBranch struct {
	When *FunctionArg
	Then *FunctionArg
}

type CaseWhenElseFunction struct {
	CaseBlock    ExpressionNode
	BranchBlocks []*CaseWhenBranch
	ElseBlock    *FunctionArg
}

func (c *CaseWhenElseFunction) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitFunctionCaseWhenElse(c)
}

func (c *CaseWhenElseFunction) Restore(flag RestoreFlag, sb *strings.Builder, args *[]int) error {
	sb.WriteString("CASE")

	if c.CaseBlock != nil {
		sb.WriteByte(' ')

		if err := c.CaseBlock.Restore(flag, sb, args); err != nil {
			return errors.WithStack(err)
		}
	}

	for _, it := range c.BranchBlocks {
		sb.WriteString(" WHEN ")

		if err := it.When.Restore(flag, sb, args); err != nil {
			return errors.WithStack(err)
		}
		sb.WriteString(" THEN ")
		if err := it.Then.Restore(flag, sb, args); err != nil {
			return errors.WithStack(err)
		}
	}

	if c.ElseBlock != nil {
		sb.WriteString(" ELSE ")
		if err := c.ElseBlock.Restore(flag, sb, args); err != nil {
			return errors.WithStack(err)
		}
	}

	sb.WriteString(" END")
	return nil
}

func (c *CaseWhenElseFunction) CntParams() (n int) {
	if c.CaseBlock != nil {
		n += c.CaseBlock.CntParams()
	}
	for _, it := range c.BranchBlocks {
		n += it.When.CntParams()
		n += it.Then.CntParams()
	}
	if c.ElseBlock != nil {
		n += c.ElseBlock.CntParams()
	}
	return
}

type CastFunction struct {
	isCast bool
	src    ExpressionNode
	cast   interface{} // *ConvertDataType or string
}

func (c *CastFunction) SetSource(src ExpressionNode) {
	c.src = src
}

func (c *CastFunction) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitFunctionCast(c)
}

func (c *CastFunction) Source() ExpressionNode {
	return c.src
}

func (c *CastFunction) GetCharset() (string, bool) {
	charset, ok := c.cast.(string)
	return charset, ok
}

func (c *CastFunction) GetCast() (*ConvertDataType, bool) {
	t, ok := c.cast.(*ConvertDataType)
	return t, ok
}

func (c *CastFunction) Restore(flag RestoreFlag, sb *strings.Builder, args *[]int) error {
	if c.isCast {
		sb.WriteString("CAST")
	} else {
		sb.WriteString("CONVERT")
	}
	sb.WriteByte('(')
	if err := c.src.Restore(flag, sb, args); err != nil {
		return errors.WithStack(err)
	}

	switch cast := c.cast.(type) {
	case string:
		sb.WriteString(" USING ")
		sb.WriteString(cast)
	case *ConvertDataType:
		if c.isCast {
			sb.WriteString(" AS ")
		} else {
			sb.WriteString(", ")
		}
		cast.writeTo(sb)
	}
	sb.WriteByte(')')

	return nil
}

func (c *CastFunction) CntParams() int {
	return c.src.CntParams()
}

const (
	_ CastType = iota
	CastToBinary
	CastToNChar
	CastToChar
	CastToDate
	CastToDateTime
	CastToTime
	CastToJson
	CastToDecimal
	CastToSigned
	CastToUnsigned
	CastToSignedInteger
	CastToUnsignedInteger
)

var _castTypeNames = [...]string{
	CastToBinary:          "BINARY",
	CastToNChar:           "NCHAR",
	CastToChar:            "CHAR",
	CastToDate:            "DATE",
	CastToDateTime:        "DATETIME",
	CastToTime:            "TIME",
	CastToJson:            "JSON",
	CastToDecimal:         "DECIMAL",
	CastToSigned:          "SIGNED",
	CastToUnsigned:        "UNSIGNED",
	CastToSignedInteger:   "SIGNED INTEGER",
	CastToUnsignedInteger: "UNSIGNED INTEGER",
}

var (
	_castRegexp     *regexp.Regexp
	_castRegexpOnce sync.Once
)

func getCastRegexp() *regexp.Regexp {
	_castRegexpOnce.Do(func() {
		_castRegexp = regexp.MustCompile(`\s*(?P<name>[a-zA-Z0-9_]+)\s*\((?P<first>[0-9]+)\s*(,\s*(?P<second>[0-9]+))?\s*\)(?P<suffix>[a-zA-Z0-9\-\s]*)$`)
	})
	return _castRegexp
}

type CastType uint8

func (c CastType) String() string {
	return _castTypeNames[c]
}

type ConvertDataType struct {
	typ                    CastType
	dimension0, dimension1 int64
	charset                string
}

func (cd *ConvertDataType) Parse(s string) error {
	var (
		typ CastType
		ok  bool
	)
	for i, it := range _castTypeNames {
		if strings.EqualFold(it, s) {
			typ = CastType(i)
			ok = true
			break
		}
	}
	if ok {
		cd.typ = typ
		return nil
	}

	subs := getCastRegexp().FindStringSubmatch(s)
	keys := getCastRegexp().SubexpNames()
	if len(subs) != len(keys) {
		return errors.Errorf("invalid cast string '%s'", s)
	}

	var name, first, second, suffix string
	for i := 1; i < len(keys); i++ {
		sub := subs[i]
		switch keys[i] {
		case "name":
			name = sub
		case "first":
			first = sub
		case "second":
			second = sub
		case "suffix":
			suffix = sub
		}
	}

	for i, it := range _castTypeNames {
		if strings.EqualFold(it, name) {
			typ = CastType(i)
			ok = true
			break
		}
	}

	if !ok {
		return errors.Errorf("invalid cast string '%s'", s)
	}

	cd.typ = typ
	cd.dimension0, _ = strconv.ParseInt(first, 10, 64)
	cd.dimension1, _ = strconv.ParseInt(second, 10, 64)
	cd.charset = strings.ToLower(strings.TrimSpace(suffix))

	for _, it := range [...]string{
		"charset",
		"character set",
	} {
		if strings.HasPrefix(cd.charset, it) {
			cd.charset = strings.TrimSpace(cd.charset[len(it):])
		}
	}
	return nil
}

func (cd *ConvertDataType) Charset() (string, bool) {
	if len(cd.charset) < 1 {
		return "", false
	}
	return cd.charset, true
}

func (cd *ConvertDataType) Dimensions() (int64, int64) {
	return cd.dimension0, cd.dimension1
}

func (cd *ConvertDataType) Type() CastType {
	return cd.typ
}

func (cd *ConvertDataType) String() string {
	var sb strings.Builder
	cd.writeTo(&sb)
	return sb.String()
}

func (cd *ConvertDataType) writeTo(sb *strings.Builder) {
	sb.WriteString(cd.typ.String())
	switch cd.typ {
	case CastToSigned, CastToUnsigned, CastToSignedInteger, CastToUnsignedInteger:
	case CastToBinary, CastToNChar:
		if cd.dimension0 != math.MinInt64 {
			sb.WriteByte('(')
			sb.WriteString(strconv.FormatInt(cd.dimension0, 10))
			sb.WriteByte(')')
		}
	case CastToChar:
		if cd.dimension0 != math.MinInt64 {
			sb.WriteByte('(')
			sb.WriteString(strconv.FormatInt(cd.dimension0, 10))
			sb.WriteByte(')')
		}
		if len(cd.charset) > 0 {
			sb.WriteString(" CHARSET ")
			sb.WriteString(cd.charset)
		}
	case CastToDecimal:
		if cd.dimension0 != math.MinInt64 && cd.dimension1 != math.MinInt64 {
			sb.WriteByte('(')
			sb.WriteString(strconv.FormatInt(cd.dimension0, 10))
			sb.WriteByte(',')
			sb.WriteString(strconv.FormatInt(cd.dimension1, 10))
			sb.WriteByte(')')
		}
	}
}
