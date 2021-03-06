// Package tagexpr is an interesting go struct tag expression syntax for field validation, etc.
//
// Copyright 2019 Bytedance Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package tagexpr

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"
)

// VM struct tag expression interpreter
type VM struct {
	tagName   string
	structJar map[string]*Struct
	rw        sync.RWMutex
}

// Struct tag expression set of struct
type Struct struct {
	vm           *VM
	name         string
	fields       map[string]*Field
	exprs        map[string]*Expr
	selectorList []string
}

// Field tag expression set of struct field
type Field struct {
	reflect.StructField
	host        *Struct
	valueGetter func(uintptr) interface{}
}

// New creates a tag expression interpreter that uses @tagName as the tag name.
func New(tagName string) *VM {
	return &VM{
		tagName:   tagName,
		structJar: make(map[string]*Struct, 256),
	}
}

// WarmUp preheating some interpreters of the struct type in batches,
// to improve the performance of the vm.Run.
func (vm *VM) WarmUp(structOrStructPtr ...interface{}) error {
	vm.rw.Lock()
	defer vm.rw.Unlock()
	for _, v := range structOrStructPtr {
		if v == nil {
			return errors.New("cannot warn up nil interface")
		}
		_, err := vm.registerStructLocked(reflect.TypeOf(v))
		if err != nil {
			return err
		}
	}
	return nil
}

// Run returns the tag expression handler of the @structPtr.
// NOTE:
//  If the structure type has not been warmed up,
//  it will be slower when it is first called.
func (vm *VM) Run(structPtr interface{}) (*TagExpr, error) {
	if structPtr == nil {
		return nil, errors.New("cannot run nil interface")
	}
	v := reflect.ValueOf(structPtr)
	if v.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("not structure pointer: %s", v.Type().String())
	}
	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("not structure pointer: %s", v.Type().String())
	}
	t := elem.Type()
	tname := t.String()
	var err error
	vm.rw.RLock()
	s, ok := vm.structJar[tname]
	vm.rw.RUnlock()
	if !ok {
		vm.rw.Lock()
		s, ok = vm.structJar[tname]
		if !ok {
			s, err = vm.registerStructLocked(t)
			if err != nil {
				vm.rw.Unlock()
				return nil, err
			}
		}
		vm.rw.Unlock()
	}
	return s.newTagExpr(v.Pointer()), nil
}

func (vm *VM) registerStructLocked(structType reflect.Type) (*Struct, error) {
	structType, err := vm.getStructType(structType)
	if err != nil {
		return nil, err
	}
	structTypeName := structType.String()
	s, had := vm.structJar[structTypeName]
	if had {
		return s, nil
	}
	s = vm.newStruct()
	vm.structJar[structTypeName] = s
	var numField = structType.NumField()
	var structField reflect.StructField
	var sub *Struct
	for i := 0; i < numField; i++ {
		structField = structType.Field(i)
		field, err := s.newField(structField)
		if err != nil {
			return nil, err
		}
		t := structField.Type
		var ptrDeep int
		for t.Kind() == reflect.Ptr {
			t = t.Elem()
			ptrDeep++
		}
		switch t.Kind() {
		default:
			field.valueGetter = func(ptr uintptr) interface{} { return nil }
		case reflect.Struct:
			sub, err = vm.registerStructLocked(field.Type)
			if err != nil {
				return nil, err
			}
			s.copySubFields(field, sub, ptrDeep)
		case reflect.Float32, reflect.Float64,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			field.setFloatGetter(t.Kind(), ptrDeep)
		case reflect.String:
			field.setStringGetter(ptrDeep)
		case reflect.Bool:
			field.setBoolGetter(ptrDeep)
		case reflect.Map, reflect.Array, reflect.Slice:
			field.setLengthGetter(ptrDeep)
		}
	}
	return s, nil
}

func (vm *VM) newStruct() *Struct {
	return &Struct{
		vm:           vm,
		fields:       make(map[string]*Field, 16),
		exprs:        make(map[string]*Expr, 64),
		selectorList: make([]string, 0, 64),
	}
}

func (s *Struct) newField(structField reflect.StructField) (*Field, error) {
	f := &Field{
		StructField: structField,
		host:        s,
	}
	err := f.parseExprs(structField.Tag.Get(s.vm.tagName))
	if err != nil {
		return nil, err
	}
	s.fields[f.Name] = f
	return f, nil
}

func (f *Field) newFrom(ptr uintptr, ptrDeep int) reflect.Value {
	v := reflect.NewAt(f.Type, unsafe.Pointer(ptr+f.Offset)).Elem()
	for i := 0; i < ptrDeep; i++ {
		v = v.Elem()
	}
	return v
}

func (f *Field) setFloatGetter(kind reflect.Kind, ptrDeep int) {
	if ptrDeep == 0 {
		f.valueGetter = func(ptr uintptr) interface{} {
			return getFloat64(kind, ptr+f.Offset)
		}
	} else {
		f.valueGetter = func(ptr uintptr) interface{} {
			v := f.newFrom(ptr, ptrDeep)
			if v.CanAddr() {
				return getFloat64(kind, v.UnsafeAddr())
			}
			return nil
		}
	}
}

func (f *Field) setBoolGetter(ptrDeep int) {
	if ptrDeep == 0 {
		f.valueGetter = func(ptr uintptr) interface{} {
			return *(*bool)(unsafe.Pointer(ptr + f.Offset))
		}
	} else {
		f.valueGetter = func(ptr uintptr) interface{} {
			v := f.newFrom(ptr, ptrDeep)
			if v.IsValid() {
				return v.Bool()
			}
			return nil
		}
	}
}

func (f *Field) setStringGetter(ptrDeep int) {
	if ptrDeep == 0 {
		f.valueGetter = func(ptr uintptr) interface{} {
			return *(*string)(unsafe.Pointer(ptr + f.Offset))
		}
	} else {
		f.valueGetter = func(ptr uintptr) interface{} {
			v := f.newFrom(ptr, ptrDeep)
			if v.IsValid() {
				return v.String()
			}
			return nil
		}
	}
}

func (f *Field) setLengthGetter(ptrDeep int) {
	f.valueGetter = func(ptr uintptr) interface{} {
		return f.newFrom(ptr, ptrDeep).Interface()
	}
}

func (f *Field) parseExprs(tag string) error {
	raw := tag
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil
	}
	if tag[0] != '{' {
		expr, err := parseExpr(tag)
		if err != nil {
			return err
		}
		selector := f.Name + "@"
		f.host.exprs[selector] = expr
		f.host.selectorList = append(f.host.selectorList, selector)
		return nil
	}
	var subtag *string
	var idx int
	var selector, exprStr string
	for {
		subtag = readPairedSymbol(&tag, '{', '}')
		if subtag != nil {
			idx = strings.Index(*subtag, ":")
			if idx > 0 {
				selector = strings.TrimSpace((*subtag)[:idx])
				switch selector {
				case "":
					continue
				case "@":
					selector = f.Name + selector
				default:
					selector = f.Name + "@" + selector
				}
				if _, had := f.host.exprs[selector]; had {
					return fmt.Errorf("duplicate expression name: %s", selector)
				}
				exprStr = strings.TrimSpace((*subtag)[idx+1:])
				if exprStr != "" {
					if expr, err := parseExpr(exprStr); err == nil {
						f.host.exprs[selector] = expr
						f.host.selectorList = append(f.host.selectorList, selector)
					} else {
						return err
					}
					trimLeftSpace(&tag)
					if tag == "" {
						return nil
					}
					continue
				}
			}
		}
		return fmt.Errorf("syntax incorrect: %q", raw)
	}
}

func (s *Struct) copySubFields(field *Field, sub *Struct, ptrDeep int) {
	nameSpace := field.Name
	for k, v := range sub.fields {
		valueGetter := v.valueGetter
		f := &Field{
			StructField: v.StructField,
			host:        v.host,
		}
		if valueGetter != nil {
			if ptrDeep == 0 {
				f.valueGetter = func(ptr uintptr) interface{} {
					return valueGetter(ptr + field.Offset)
				}
			} else {
				f.valueGetter = func(ptr uintptr) interface{} {
					newField := reflect.NewAt(field.Type, unsafe.Pointer(ptr+field.Offset))
					for i := 0; i < ptrDeep; i++ {
						newField = newField.Elem()
					}
					return valueGetter(uintptr(newField.Pointer()))
				}
			}
		}
		s.fields[nameSpace+"."+k] = f
	}
	var selector string
	for k, v := range sub.exprs {
		selector = nameSpace + "." + k
		s.exprs[selector] = v
		s.selectorList = append(s.selectorList, selector)
	}
}

func (vm *VM) getStructType(t reflect.Type) (reflect.Type, error) {
	structType := t
	for structType.Kind() == reflect.Ptr {
		structType = structType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("not structure pointer or structure: %s", t.String())
	}
	return structType, nil
}

func (s *Struct) newTagExpr(ptr uintptr) *TagExpr {
	te := &TagExpr{
		s:   s,
		ptr: ptr,
	}
	return te
}

// TagExpr struct tag expression evaluator
type TagExpr struct {
	s   *Struct
	ptr uintptr
}

// EvalFloat evaluate the value of the struct tag expression by the selector expression.
// NOTE:
//  If the expression value type is not float64, return 0.
func (t *TagExpr) EvalFloat(selector string) float64 {
	r, _ := t.Eval(selector).(float64)
	return r
}

// EvalString evaluate the value of the struct tag expression by the selector expression.
// NOTE:
//  If the expression value type is not string, return "".
func (t *TagExpr) EvalString(selector string) string {
	r, _ := t.Eval(selector).(string)
	return r
}

// EvalBool evaluate the value of the struct tag expression by the selector expression.
// NOTE:
//  If the expression value type is not bool, return false.
func (t *TagExpr) EvalBool(selector string) bool {
	r, _ := t.Eval(selector).(bool)
	return r
}

// Eval evaluate the value of the struct tag expression by the selector expression.
// NOTE:
//  format: fieldName, fieldName.exprName, fieldName1.fieldName2.exprName1
//  result types: float64, string, bool, nil
func (t *TagExpr) Eval(selector string) interface{} {
	expr, ok := t.s.exprs[selector]
	if !ok {
		return nil
	}
	return expr.run(getFieldSelector(selector), t)
}

// Range loop through each tag expression
// NOTE:
//  eval result types: float64, string, bool, nil
func (t *TagExpr) Range(fn func(selector string, eval func() interface{}) bool) {
	exprs := t.s.exprs
	for _, selector := range t.s.selectorList {
		if !fn(selector, func() interface{} {
			return exprs[selector].run(getFieldSelector(selector), t)
		}) {
			return
		}
	}
}

func (t *TagExpr) getValue(field string, subFields []interface{}) (v interface{}) {
	f, ok := t.s.fields[field]
	if !ok {
		return nil
	}
	if f.valueGetter == nil {
		return nil
	}
	v = f.valueGetter(t.ptr)
	if v == nil {
		return nil
	}
	if len(subFields) == 0 {
		return v
	}
	vv := reflect.ValueOf(v)
	for _, k := range subFields {
		for vv.Kind() == reflect.Ptr {
			vv = vv.Elem()
		}
		switch vv.Kind() {
		case reflect.Slice, reflect.Array, reflect.String:
			if float, ok := k.(float64); ok {
				idx := int(float)
				if idx >= vv.Len() {
					return nil
				}
				vv = vv.Index(idx)
			} else {
				return nil
			}
		case reflect.Map:
			k := safeConvert(reflect.ValueOf(k), vv.Type().Key())
			if !k.IsValid() {
				return nil
			}
			vv = vv.MapIndex(k)
		default:
			return nil
		}
	}
	for vv.Kind() == reflect.Ptr {
		vv = vv.Elem()
	}
	switch vv.Kind() {
	default:
		if !vv.IsNil() && vv.CanInterface() {
			return vv.Interface()
		}
		return nil
	case reflect.String:
		return vv.String()
	case reflect.Bool:
		return vv.Bool()
	case reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if vv.CanAddr() {
			return getFloat64(vv.Kind(), vv.UnsafeAddr())
		}
		return vv.Convert(float64Type).Float()
	}
}

func safeConvert(v reflect.Value, t reflect.Type) reflect.Value {
	defer func() { recover() }()
	return v.Convert(t)
}

var float64Type = reflect.TypeOf(float64(0))

func getFieldSelector(selector string) string {
	idx := strings.Index(selector, "@")
	if idx == -1 {
		return selector
	}
	return selector[:idx]
}

func getFloat64(kind reflect.Kind, ptr uintptr) interface{} {
	p := unsafe.Pointer(ptr)
	switch kind {
	case reflect.Float32:
		return float64(*(*float32)(p))
	case reflect.Float64:
		return *(*float64)(p)
	case reflect.Int:
		return float64(*(*int)(p))
	case reflect.Int8:
		return float64(*(*int8)(p))
	case reflect.Int16:
		return float64(*(*int16)(p))
	case reflect.Int32:
		return float64(*(*int32)(p))
	case reflect.Int64:
		return float64(*(*int64)(p))
	case reflect.Uint:
		return float64(*(*uint)(p))
	case reflect.Uint8:
		return float64(*(*uint8)(p))
	case reflect.Uint16:
		return float64(*(*uint16)(p))
	case reflect.Uint32:
		return float64(*(*uint32)(p))
	case reflect.Uint64:
		return float64(*(*uint64)(p))
	case reflect.Uintptr:
		return float64(*(*uintptr)(p))
	}
	return nil
}
