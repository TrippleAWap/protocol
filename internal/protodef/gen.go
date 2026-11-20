package protodef

import (
	"fmt"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// gen walks Marshal bodies as the Reader runs them and builds ProtoDef types.
type gen struct {
	p      *program
	ioType types.Type
	reader types.Type

	// named holds the shared types in the order they were first used.
	named *Map
	// names maps the object a named type comes from to its name.
	names map[any]string
	// owners maps each type name to the object it comes from.
	owners map[string]any
	// lookups caches the tables of lookup functions such as lookupEvent.
	lookups map[*types.Func]*lookupTable
	// inlining is the stack of functions being inlined, to catch recursion.
	inlining map[*types.Func]bool

	diags []string
	// stack holds the names of the types being built.
	stack []string
	// strings maps type name, then field name, to the only strings the Reader accepts.
	strings map[string]map[string][]string
	// consts holds the constants each field is compared with, and uses the switches on
	// fields, to give those fields mappers (see mapper.go).
	consts map[*Field]map[*types.Const]bool
	uses   []switchUse
	// goFields maps a type name, then the Go field path a field of the type is read
	// into, to that field. The test uses it to compare values with the Go structs.
	goFields map[string]map[string]*Field
	// pending holds scopes made outside any type, whose diagnostics are added at the end.
	pending []*scope
}

func (g *gen) diag(sc *scope, pos token.Pos, format string, args ...any) {
	sc.diags = append(sc.diags, fmt.Sprintf("%s: %s", g.p.fset.Position(pos), fmt.Sprintf(format, args...)))
}

// named returns the name of the shared type made by build for key, building it the
// first time. The name is reserved before building, so recursive types refer to it.
func (g *gen) namedType(key any, name string, build func(sc *scope)) string {
	if n, ok := g.names[key]; ok {
		return n
	}
	base := name
	for i := 2; ; i++ {
		if _, taken := g.owners[name]; !taken {
			break
		}
		name = base + strconv.Itoa(i)
	}
	g.names[key] = name
	g.owners[name] = key
	g.named.Set(name, nil)
	sc := newScope(nil, true)
	g.stack = append(g.stack, name)
	build(sc)
	g.stack = g.stack[:len(g.stack)-1]
	g.named.Set(name, container(sc.fields))
	g.diags = append(g.diags, sc.diags...)
	return name
}

// fixedType registers a type that is written by hand.
func (g *gen) fixedType(key any, name string, build func() any) string {
	if n, ok := g.names[key]; ok {
		return n
	}
	g.names[key] = name
	g.owners[name] = key
	g.named.Set(name, nil)
	g.named.Set(name, build())
	return name
}

// methodType returns the named type read by a Marshal method.
func (g *gen) methodType(fn *types.Func) string {
	return g.namedType(fn, typeName(fn), func(sc *scope) {
		g.body(fn, map[int]*binding{-1: {place: "$"}, 0: {io: true}}, sc, "")
	})
}

// typeName names the type of a method's receiver, or a package-level function.
func typeName(fn *types.Func) string {
	name := fn.Name()
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		t := sig.Recv().Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			name = n.Obj().Name()
		}
	}
	return exported(name)
}

func exported(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// marshalerType returns the named type of t, read by the Marshal method of *t.
func (g *gen) marshalerType(t types.Type, sc *scope, pos token.Pos) any {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	ms := types.NewMethodSet(types.NewPointer(t))
	sel := ms.Lookup(g.p.root, "Marshal")
	if sel == nil {
		if n, ok := t.(*types.Named); ok {
			sel = ms.Lookup(n.Obj().Pkg(), "Marshal")
		}
	}
	if sel == nil {
		g.diag(sc, pos, "type %s has no Marshal method", t)
		return "void"
	}
	fn, ok := sel.Obj().(*types.Func)
	if !ok {
		g.diag(sc, pos, "Marshal of %s is not a method", t)
		return "void"
	}
	return g.methodType(fn)
}

// funcType returns the named type read by a helper func(IO, *T, ...).
func (g *gen) funcType(fn *types.Func) string {
	name := fn.Name()
	// marshalDataStorePropertyValue reads a DataStorePropertyValue.
	if rest, ok := strings.CutPrefix(name, "marshal"); ok && rest != "" {
		name = rest
	}
	return g.namedType(fn, exported(name), func(sc *scope) {
		g.body(fn, map[int]*binding{0: {io: true}, 1: {place: "$"}}, sc, "")
	})
}

// collect builds the type read by run: void for nothing, the field's own type for one
// field, and a container for more.
func (g *gen) collect(sc *scope, run func(*scope)) any {
	probe := newScope(sc, true)
	run(probe)
	if len(probe.fields) == 1 && !probe.fields[0].Anon {
		flat := newScope(sc, false)
		run(flat)
		if len(flat.fields) == 1 && !flat.fields[0].Anon {
			sc.diags = append(sc.diags, flat.diags...)
			return flat.fields[0].Type
		}
	}
	sc.diags = append(sc.diags, probe.diags...)
	if len(probe.fields) == 0 {
		return "void"
	}
	return container(probe.fields)
}
