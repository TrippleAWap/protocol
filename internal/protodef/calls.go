package protodef

import (
	"go/ast"
	"go/token"
	"go/types"
)

// hasIOArg reports whether any argument of c is the IO.
func (g *gen) hasIOArg(c *ast.CallExpr, e *env) bool {
	for _, a := range c.Args {
		if g.isIO(a, e) {
			return true
		}
	}
	return false
}

// ioCall runs a method called on the IO.
func (g *gen) ioCall(name string, c *ast.CallExpr, e *env, sc *scope) {
	if isIOIgnored(name) {
		if isIOError(name) {
			sc.errored = true
		}
		return
	}
	if len(c.Args) == 0 {
		g.diag(sc, c.Pos(), "IO method %s without arguments", name)
		return
	}
	if fieldName, ok := ioInline(name); ok {
		fn := g.readerMethod(name)
		if fn == nil {
			g.diag(sc, c.Pos(), "no Reader method %s", name)
			return
		}
		g.inlineFunc(fn, nil, c.Args, e, sc, fieldName)
		return
	}
	place, ok := g.target(c.Args[0], e)
	if !ok {
		g.diag(sc, c.Pos(), "cannot tell what %s reads into", name)
		return
	}
	typ, ok := g.ioMethodType(name, c.Args, sc, c.Pos())
	if ok {
		g.add(sc, e, place, typ)
		return
	}
	// A Reader method outside the IO interface: part of the caller's encoding.
	fn := g.readerMethod(name)
	if fn == nil {
		g.diag(sc, c.Pos(), "unknown IO method %s", name)
		return
	}
	g.inlineFunc(fn, nil, c.Args, e, sc, "")
}

// ioMethodType returns the type that the IO method name reads, for methods in the IO
// interface. It reports false for Reader methods outside it.
func (g *gen) ioMethodType(name string, args []ast.Expr, sc *scope, pos token.Pos) (any, bool) {
	if t, ok := ioPrimitive(name); ok {
		return t, true
	}
	switch name {
	case "NBT", "NBTList":
		if len(args) < 2 {
			g.diag(sc, pos, "NBT call without an encoding")
			return "nbt", true
		}
		var enc string
		switch a := unparen(args[1]).(type) {
		case *ast.SelectorExpr:
			enc = a.Sel.Name
		case *ast.Ident:
			enc = a.Name
		}
		switch enc {
		case "NetworkLittleEndian":
			return "nbt", true
		case "LittleEndian":
			return "lnbt", true
		}
		g.diag(sc, pos, "NBT encoding %q", enc)
		return "nbt", true
	}
	if o, ok := g.overrides[name]; ok {
		return g.fixedType("override:"+name, name, func() any { return o(g) }), true
	}
	fn := g.readerMethod(name)
	if fn == nil || !g.inIOInterface(name) {
		return nil, false
	}
	// A Reader method: the receiver is the IO, the first argument the value read.
	return g.namedBody(fn, name, map[int]*binding{-1: {io: true}, 0: {place: "$"}}), true
}

func (g *gen) readerMethod(name string) *types.Func {
	o, _, _ := types.LookupFieldOrMethod(g.reader, true, g.p.root, name)
	fn, _ := o.(*types.Func)
	return fn
}

func (g *gen) inIOInterface(name string) bool {
	iface, ok := g.ioType.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	for i := 0; i < iface.NumMethods(); i++ {
		if iface.Method(i).Name() == name {
			return true
		}
	}
	return false
}

// methodCall runs a call of a method on a value that is not the IO.
func (g *gen) methodCall(fn *types.Func, recv ast.Expr, c *ast.CallExpr, e *env, sc *scope) {
	if !g.hasIOArg(c, e) {
		for _, a := range c.Args {
			g.exprCalls(a, e, sc)
		}
		return
	}
	place, ok := g.place(recv, e)
	if !ok {
		g.diag(sc, c.Pos(), "cannot tell the receiver of %s", fn.Name())
		return
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		g.diag(sc, c.Pos(), "%s is not a method", fn.Name())
		return
	}
	if _, isIface := sig.Recv().Type().Underlying().(*types.Interface); isIface {
		g.dynamicCall(place, c, sc)
		return
	}
	if fn.Name() == "Marshal" {
		g.add(sc, e, place, g.methodType(fn))
		return
	}
	g.inlineFunc(fn, recv, c.Args, e, sc, "")
}

// dynamicCall reads an interface value whose concrete type was picked by a lookup
// function from a field read before.
func (g *gen) dynamicCall(place string, c *ast.CallExpr, sc *scope) {
	d := g.dyn(place, sc)
	if d == nil {
		g.diag(sc, c.Pos(), "interface value %s read without a known type", place)
		return
	}
	ref, ok := g.ref(d.idPlace, sc)
	if !ok {
		g.diag(sc, c.Pos(), "type of %s comes from a value not on the wire", place)
		return
	}
	fields := NewMap()
	for _, k := range d.table.keys {
		fields.Set(k, g.marshalerType(d.table.types[k], sc, c.Pos()))
		g.noteConst(d.idPlace, d.table.consts[k], sc)
	}
	g.noteSwitch(d.idPlace, fields, sc)
	g.add(sc, nil, place, switchOf(ref, fields, nil))
}

// funcCall runs a call of a package-level function.
func (g *gen) funcCall(fn *types.Func, c *ast.CallExpr, e *env, sc *scope) {
	if fn.Pkg() == g.p.root && g.helper(fn.Name(), c, e, sc) {
		return
	}
	if !g.hasIOArg(c, e) {
		if t := g.lookupTable(fn); t != nil && !t.returns && len(c.Args) >= 2 {
			idPlace, ok1 := g.place(c.Args[0], e)
			target, ok2 := g.place(c.Args[1], e)
			if ok1 && ok2 {
				sc.dyn[target] = &dynamic{idPlace: idPlace, table: t}
			}
			return
		}
		for _, a := range c.Args {
			g.exprCalls(a, e, sc)
		}
		return
	}
	if fn.Pkg() != g.p.root && fn.Pkg() != g.p.packet {
		g.diag(sc, c.Pos(), "call of %s from another package", fn.FullName())
		return
	}
	// A helper of the form func(IO, *T, ...) reads a T: a type of its own.
	sig, ok := fn.Type().(*types.Signature)
	if ok && sig.Params().Len() >= 2 && len(c.Args) >= 2 && g.isIO(c.Args[0], e) {
		if ptr, ok := sig.Params().At(1).Type().(*types.Pointer); ok && !isBasic(ptr.Elem()) {
			if place, ok := g.target(c.Args[1], e); ok {
				name := g.funcType(fn)
				if !g.emptyType(name) {
					g.add(sc, e, place, name)
				}
				return
			}
		}
	}
	g.inlineFunc(fn, nil, c.Args, e, sc, "")
}

// emptyType reports whether the named type reads nothing, as for a helper that only
// checks a value. A type still being built is not empty.
func (g *gen) emptyType(name string) bool {
	v, ok := g.named.Get(name)
	if !ok || v == nil {
		return false
	}
	c, ok := v.([]any)
	if !ok || len(c) != 2 {
		return false
	}
	fields, ok := c[1].([]*Field)
	return ok && len(fields) == 0
}

// isBasic reports whether t is a number, string or bool. A helper that reads one, such as
// playerListAction(IO, *byte), is part of its caller's container: the caller often
// branches on the value it sets.
func isBasic(t types.Type) bool {
	_, ok := t.Underlying().(*types.Basic)
	return ok
}
