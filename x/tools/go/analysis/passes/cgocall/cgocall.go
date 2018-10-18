// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package cgocall defines an Analyzer that detects some violations of
// the cgo pointer passing rules.
package cgocall

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/analysis/passes/internal/analysisutil"
	"golang.org/x/tools/go/ast/inspector"
)

const Doc = `detect some violations of the cgo pointer passing rules

Check for invalid cgo pointer passing.
This looks for code that uses cgo to call C code passing values
whose types are almost always invalid according to the cgo pointer
sharing rules.
Specifically, it warns about attempts to pass a Go chan, map, func,
or slice to C, either directly, or via a pointer, array, or struct.`

var Analyzer = &analysis.Analyzer{
	Name:             "cgocall",
	Doc:              Doc,
	Requires:         []*analysis.Analyzer{inspect.Analyzer},
	RunDespiteErrors: true,
	Run:              run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{
		(*ast.CallExpr)(nil),
	}
	inspect.WithStack(nodeFilter, func(n ast.Node, push bool, stack []ast.Node) bool {
		if !push {
			return true
		}
		call, name := findCall(pass.Fset, stack)
		if call == nil {
			return true // not a call we need to check
		}

		// A call to C.CBytes passes a pointer but is always safe.
		if name == "CBytes" {
			return true
		}

		if false {
			fmt.Printf("%s: inner call to C.%s\n", pass.Fset.Position(n.Pos()), name)
			fmt.Printf("%s: outer call to C.%s\n", pass.Fset.Position(call.Lparen), name)
		}

		for _, arg := range call.Args {
			if !typeOKForCgoCall(cgoBaseType(pass.TypesInfo, arg), make(map[types.Type]bool)) {
				pass.Reportf(arg.Pos(), "possibly passing Go type with embedded pointer to C")
				break
			}

			// Check for passing the address of a bad type.
			if conv, ok := arg.(*ast.CallExpr); ok && len(conv.Args) == 1 &&
				isUnsafePointer(pass.TypesInfo, conv.Fun) {
				arg = conv.Args[0]
			}
			if u, ok := arg.(*ast.UnaryExpr); ok && u.Op == token.AND {
				if !typeOKForCgoCall(cgoBaseType(pass.TypesInfo, u.X), make(map[types.Type]bool)) {
					pass.Reportf(arg.Pos(), "possibly passing Go type with embedded pointer to C")
					break
				}
			}
		}
		return true
	})
	return nil, nil
}

// findCall returns the CallExpr that we need to check, which may not be
// the same as the one we're currently visiting, due to code generation.
// It also returns the name of the function, such as "f" for C.f(...).
//
// This checker was initially written in vet to inpect unprocessed cgo
// source files using partial type information. However, Analyzers in
// the new analysis API are presented with the type-checked, processed
// Go ASTs resulting from cgo processing files, so we must choose
// between:
//
// a) locating the cgo file (e.g. from //line directives)
//    and working with that, or
// b) working with the file generated by cgo.
//
// We cannot use (a) because it does not provide type information, which
// the analyzer needs, and it is infeasible for the analyzer to run the
// type checker on this file. Thus we choose (b), which is fragile,
// because the checker may need to change each time the cgo processor
// changes.
//
// Consider a cgo source file containing this header:
//
// 	 /* void f(void *x, *y); */
//	 import "C"
//
// The cgo tool expands a call such as:
//
// 	 C.f(x, y)
//
// to this:
//
// 1	func(param0, param1 unsafe.Pointer) {
// 2		... various checks on params ...
// 3		(_Cfunc_f)(param0, param1)
// 4	}(x, y)
//
// We first locate the _Cfunc_f call on line 3, then
// walk up the stack of enclosing nodes until we find
// the call on line 4.
//
func findCall(fset *token.FileSet, stack []ast.Node) (*ast.CallExpr, string) {
	last := len(stack) - 1
	call := stack[last].(*ast.CallExpr)
	if id, ok := analysisutil.Unparen(call.Fun).(*ast.Ident); ok {
		if name := strings.TrimPrefix(id.Name, "_Cfunc_"); name != id.Name {
			// Find the outer call with the arguments (x, y) we want to check.
			for i := last - 1; i >= 0; i-- {
				if outer, ok := stack[i].(*ast.CallExpr); ok {
					return outer, name
				}
			}
			// This shouldn't happen.
			// Perhaps the code generator has changed?
			log.Printf("%s: can't find outer call for C.%s(...)",
				fset.Position(call.Lparen), name)
		}
	}
	return nil, ""
}

// cgoBaseType tries to look through type conversions involving
// unsafe.Pointer to find the real type. It converts:
//   unsafe.Pointer(x) => x
//   *(*unsafe.Pointer)(unsafe.Pointer(&x)) => x
func cgoBaseType(info *types.Info, arg ast.Expr) types.Type {
	switch arg := arg.(type) {
	case *ast.CallExpr:
		if len(arg.Args) == 1 && isUnsafePointer(info, arg.Fun) {
			return cgoBaseType(info, arg.Args[0])
		}
	case *ast.StarExpr:
		call, ok := arg.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			break
		}
		// Here arg is *f(v).
		t := info.Types[call.Fun].Type
		if t == nil {
			break
		}
		ptr, ok := t.Underlying().(*types.Pointer)
		if !ok {
			break
		}
		// Here arg is *(*p)(v)
		elem, ok := ptr.Elem().Underlying().(*types.Basic)
		if !ok || elem.Kind() != types.UnsafePointer {
			break
		}
		// Here arg is *(*unsafe.Pointer)(v)
		call, ok = call.Args[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			break
		}
		// Here arg is *(*unsafe.Pointer)(f(v))
		if !isUnsafePointer(info, call.Fun) {
			break
		}
		// Here arg is *(*unsafe.Pointer)(unsafe.Pointer(v))
		u, ok := call.Args[0].(*ast.UnaryExpr)
		if !ok || u.Op != token.AND {
			break
		}
		// Here arg is *(*unsafe.Pointer)(unsafe.Pointer(&v))
		return cgoBaseType(info, u.X)
	}

	return info.Types[arg].Type
}

// typeOKForCgoCall reports whether the type of arg is OK to pass to a
// C function using cgo. This is not true for Go types with embedded
// pointers. m is used to avoid infinite recursion on recursive types.
func typeOKForCgoCall(t types.Type, m map[types.Type]bool) bool {
	if t == nil || m[t] {
		return true
	}
	m[t] = true
	switch t := t.Underlying().(type) {
	case *types.Chan, *types.Map, *types.Signature, *types.Slice:
		return false
	case *types.Pointer:
		return typeOKForCgoCall(t.Elem(), m)
	case *types.Array:
		return typeOKForCgoCall(t.Elem(), m)
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if !typeOKForCgoCall(t.Field(i).Type(), m) {
				return false
			}
		}
	}
	return true
}

func isUnsafePointer(info *types.Info, e ast.Expr) bool {
	t := info.Types[e].Type
	return t != nil && t.Underlying() == types.Typ[types.UnsafePointer]
}
