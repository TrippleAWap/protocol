package protodef

import (
	"fmt"
	"go/ast"
	"go/constant"
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

// binding is what a local variable or parameter stands for.
type binding struct {
	place string
	// copy is set for a local holding a copy of the value at place.
	copy  bool
	io    bool
	fn    *ast.FuncLit
	fnEnv *env
}

// env holds the bindings of one function body.
type env struct {
	vars   map[types.Object]*binding
	parent *env
	// ioName names the first local field read while inlining an IO method listed in ioInline.
	ioName string
}

func newEnv(parent *env) *env { return &env{vars: map[types.Object]*binding{}, parent: parent} }

func (e *env) lookup(o types.Object) *binding {
	for ; e != nil; e = e.parent {
		if b, ok := e.vars[o]; ok {
			return b
		}
	}
	return nil
}

// scope is a ProtoDef container being built.
type scope struct {
	parent *scope
	// level is true for scopes that are their own container in the schema, so that a
	// reference from inside to a field outside needs "../".
	level  bool
	fields []*Field
	used   map[string]int
	// places maps the Go value read into a field to the name of the field.
	places map[string]string
	// dyn holds what decides the concrete type of interface values read so far.
	dyn map[string]*dynamic
	// facts holds values that are not read but set from constants, possibly depending
	// on a field read before (see derive.go). A place set here is not a wire field any
	// more: places holds "" for it.
	facts map[string]*fact
	diags []string
	// errored is set when the scope reports invalid data (UnknownEnumOption and the like).
	errored bool
}

func newScope(parent *scope, level bool) *scope {
	return &scope{
		parent: parent, level: level, used: map[string]int{}, places: map[string]string{},
		dyn: map[string]*dynamic{}, facts: map[string]*fact{},
	}
}

// dynamic says that an interface value gets its concrete type from a field read before.
type dynamic struct {
	idPlace string
	table   *lookupTable
}

// lookupTable is the switch of a lookup function: case value to concrete type.
type lookupTable struct {
	keys  []string
	types map[string]types.Type
	// returns is set for lookups that return the value instead of storing it.
	returns bool
	// consts holds the constant each case names.
	consts map[string]*types.Const
}

func (g *gen) diag(sc *scope, pos token.Pos, format string, args ...any) {
	sc.diags = append(sc.diags, fmt.Sprintf("%s: %s", g.p.fset.Position(pos), fmt.Sprintf(format, args...)))
}

// add appends a field read into place to sc.
func (g *gen) add(sc *scope, e *env, place string, typ any) {
	g.addField(sc, e, place, typ, false)
}

// addField adds a field like add. converted is set for a value stored in a Go type other
// than the one read (IntegerFunc), which the test compares by bits only.
func (g *gen) addField(sc *scope, e *env, place string, typ any, converted bool) {
	name := placeName(place)
	if e != nil && e.ioName != "" && strings.HasPrefix(place, "L:") {
		name = e.ioName
		e.ioName = ""
	}
	f := &Field{Name: sc.uniq(name), Type: typ}
	sc.fields = append(sc.fields, f)
	if place != "" {
		sc.places[place] = f.Name
		sc.facts[place] = nil
	}
	if path, ok := strings.CutPrefix(place, "$."); ok && len(g.stack) > 0 && isSelectorPath(path) {
		typ := g.stack[len(g.stack)-1]
		if g.goFields[typ] == nil {
			g.goFields[typ] = map[string]*Field{}
		}
		// A value read twice ends up holding what was read last.
		delete(g.goFields[typ], path)
		delete(g.goFields[typ], "~"+path)
		if converted {
			path = "~" + path
		}
		g.goFields[typ][path] = f
	}
}

// isSelectorPath reports whether path is a chain of field names, such as Stack.Count.
func isSelectorPath(path string) bool {
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

func (sc *scope) uniq(name string) string {
	if name == "" {
		name = "value"
	}
	n := sc.used[name]
	sc.used[name] = n + 1
	if n == 0 {
		return name
	}
	return name + "_" + strconv.Itoa(n+1)
}

// ref returns the ProtoDef path of the field holding place, as seen from sc.
func (g *gen) ref(place string, sc *scope) (string, bool) {
	up := 0
	for s := sc; s != nil; s = s.parent {
		if name, ok := s.places[place]; ok {
			if name == "" {
				return "", false
			}
			return strings.Repeat("../", up) + name, true
		}
		if s.level {
			up++
		}
	}
	return "", false
}

func (g *gen) dyn(place string, sc *scope) (*dynamic, *scope) {
	for s := sc; s != nil; s = s.parent {
		if d, ok := s.dyn[place]; ok {
			return d, s
		}
	}
	return nil, nil
}

// commit moves the places, dynamic types and diagnostics of a finished child scope into
// its parent. The fields of the child stay where they are: they end up in a container
// or type of their own, but a later field can still refer to them by name, as ProtoDef
// merges anonymous switches into the parent.
func (sc *scope) commit(child *scope, rename func(string) string) {
	for p, n := range child.places {
		if rename != nil {
			n = rename(n)
		}
		if n != "" {
			sc.places[p] = n
		}
	}
	for p, d := range child.dyn {
		sc.dyn[p] = d
	}
	sc.diags = append(sc.diags, child.diags...)
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

// isIO reports whether x is the IO being read from: a protocol.IO or a *protocol.Reader.
func (g *gen) isIO(x ast.Expr, e *env) bool {
	if id, ok := unparen(x).(*ast.Ident); ok {
		if b := e.lookup(g.obj(id)); b != nil {
			return b.io
		}
	}
	t := g.p.info.TypeOf(x)
	return t != nil && (types.Identical(t, g.ioType) || types.Identical(t, g.reader))
}

func (g *gen) obj(id *ast.Ident) types.Object {
	if o := g.p.info.Uses[id]; o != nil {
		return o
	}
	return g.p.info.Defs[id]
}

// place returns a key naming the Go value x refers to.
func (g *gen) place(x ast.Expr, e *env) (string, bool) {
	switch x := unparen(x).(type) {
	case *ast.Ident:
		o := g.obj(x)
		if o == nil {
			return "", false
		}
		if b := e.lookup(o); b != nil && b.place != "" {
			return b.place, true
		}
		if v, ok := o.(*types.Var); ok {
			if v.Parent() == v.Pkg().Scope() {
				return "G:" + v.Name(), true
			}
			return fmt.Sprintf("L:%s@%d", v.Name(), v.Pos()), true
		}
		return "", false
	case *ast.SelectorExpr:
		if sel, ok := g.p.info.Selections[x]; ok && sel.Kind() == types.FieldVal {
			base, ok := g.place(x.X, e)
			if !ok {
				return "", false
			}
			return base + "." + x.Sel.Name, true
		}
		if o, ok := g.p.info.Uses[x.Sel].(*types.Var); ok {
			return "G:" + o.Name(), true
		}
		return "", false
	case *ast.StarExpr:
		return g.place(x.X, e)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return g.place(x.X, e)
		}
	case *ast.IndexExpr:
		base, ok := g.place(x.X, e)
		if !ok {
			return "", false
		}
		if v := g.p.info.Types[x.Index].Value; v != nil {
			return base + "[" + v.ExactString() + "]", true
		}
		return base + "[]", true
	case *ast.SliceExpr:
		return g.place(x.X, e)
	case *ast.CallExpr:
		if tv, ok := g.p.info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
			return g.place(x.Args[0], e)
		}
	}
	return "", false
}

// target returns the place a value is read into. A local holding a copy of another
// value stops standing for it once something is read into the copy.
func (g *gen) target(x ast.Expr, e *env) (string, bool) {
	inner := unparen(x)
	if u, ok := inner.(*ast.UnaryExpr); ok && u.Op == token.AND {
		inner = unparen(u.X)
	}
	if id, ok := inner.(*ast.Ident); ok {
		if o := g.obj(id); o != nil {
			if b := e.lookup(o); b != nil && b.copy {
				fresh := fmt.Sprintf("L:%s@%d", o.Name(), o.Pos())
				e.vars[o] = &binding{place: fresh}
				return fresh, true
			}
		}
	}
	return g.place(x, e)
}

// placeName makes a field name from the last part of a place.
func placeName(place string) string {
	if place == "" {
		return ""
	}
	if strings.HasPrefix(place, "L:") || strings.HasPrefix(place, "G:") {
		place = place[2:]
		if i := strings.IndexByte(place, '@'); i >= 0 {
			rest := place[i:]
			place = place[:i]
			// A local's field: keep what follows the position.
			if j := strings.IndexByte(rest, '.'); j >= 0 {
				place = rest[j+1:]
			}
		}
	}
	if i := strings.LastIndexByte(place, '.'); i >= 0 {
		place = place[i+1:]
	}
	idx := ""
	if i := strings.IndexByte(place, '['); i >= 0 {
		for _, part := range strings.Split(place[i:], "[") {
			part = strings.TrimSuffix(part, "]")
			if part != "" {
				idx += "_" + part
			}
		}
		place = place[:i]
	}
	if place == "$" {
		place = ""
	}
	name := strings.TrimPrefix(snake(place)+idx, "_")
	// An element of the value itself, such as x[0] of a [3]int8: ProtoDef compilers use
	// field names as identifiers, which cannot start with a digit.
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		name = "value_" + name
	}
	return name
}

// snake turns a Go identifier into snake_case: MoveActorAbsolute -> move_actor_absolute,
// NPCRequest -> npc_request, EntityRuntimeIDs -> entity_runtime_ids.
func snake(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		upper := r >= 'A' && r <= 'Z'
		if upper && i > 0 {
			prev := rs[i-1]
			prevLower := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
			nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
			// "IDs" is the plural of ID, not I + Ds.
			pluralS := i+1 < len(rs) && rs[i+1] == 's' && (i+2 == len(rs) || (rs[i+2] >= 'A' && rs[i+2] <= 'Z'))
			if prevLower || (nextLower && !pluralS && prev >= 'A' && prev <= 'Z') {
				b.WriteByte('_')
			}
		}
		if upper {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unparen(x ast.Expr) ast.Expr {
	for {
		p, ok := x.(*ast.ParenExpr)
		if !ok {
			return x
		}
		x = p.X
	}
}

// stmts runs a statement list. It reports whether the list ends in a return.
func (g *gen) stmts(list []ast.Stmt, e *env, sc *scope) bool {
	for i, s := range list {
		switch s := s.(type) {
		case *ast.ReturnStmt:
			for _, r := range s.Results {
				g.exprCalls(r, e, sc)
			}
			return true
		case *ast.IfStmt:
			consumed, term := g.ifStmt(s, list[i+1:], e, sc)
			if consumed {
				return term
			}
			if term {
				return true
			}
		default:
			if g.stmt(s, e, sc) {
				return true
			}
		}
	}
	return false
}

func (g *gen) stmt(s ast.Stmt, e *env, sc *scope) bool {
	switch s := s.(type) {
	case *ast.ExprStmt:
		g.exprCalls(s.X, e, sc)
	case *ast.AssignStmt:
		if len(s.Rhs) == 1 && g.lookupAssign(s.Lhs[0], s.Rhs[0], e, sc) {
			return false
		}
		for _, r := range s.Rhs {
			g.exprCalls(r, e, sc)
		}
		if len(s.Lhs) == len(s.Rhs) {
			for i, l := range s.Lhs {
				g.bind(l, s.Rhs[i], e, s.Tok == token.DEFINE)
				if v := g.p.info.Types[s.Rhs[i]].Value; v != nil {
					g.setConst(l, v, e, sc)
				} else if s.Tok == token.ASSIGN && !isIdent(l) {
					g.alias(l, s.Rhs[i], e, sc)
				}
			}
		}
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return false
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for _, v := range vs.Values {
				g.exprCalls(v, e, sc)
			}
			if len(vs.Values) == len(vs.Names) {
				for i, n := range vs.Names {
					g.bind(n, vs.Values[i], e, true)
					if v := g.p.info.Types[vs.Values[i]].Value; v != nil {
						g.setConst(n, v, e, sc)
					}
				}
			}
		}
	case *ast.BlockStmt:
		return g.stmts(s.List, e, sc)
	case *ast.SwitchStmt:
		g.switchStmt(s, e, sc)
	case *ast.ForStmt:
		g.forStmt(s, e, sc)
	case *ast.RangeStmt:
		g.rangeStmt(s, e, sc)
	case *ast.LabeledStmt:
		return g.stmt(s.Stmt, e, sc)
	case *ast.TypeSwitchStmt:
		probe := newScope(sc, true)
		g.stmts(s.Body.List, newEnv(e), probe)
		if len(probe.fields) > 0 {
			g.diag(sc, s.Pos(), "type switch that reads fields")
		}
	case *ast.IncDecStmt, *ast.BranchStmt, *ast.EmptyStmt, *ast.DeferStmt, *ast.GoStmt, *ast.SendStmt:
	default:
		g.diag(sc, s.Pos(), "unsupported statement %T", s)
	}
	return false
}

// bind records what a local assigned from rhs stands for.
func (g *gen) bind(lhs, rhs ast.Expr, e *env, define bool) {
	id, ok := unparen(lhs).(*ast.Ident)
	if !ok || id.Name == "_" {
		return
	}
	o := g.obj(id)
	if o == nil {
		return
	}
	if fl, ok := unparen(rhs).(*ast.FuncLit); ok {
		e.vars[o] = &binding{fn: fl, fnEnv: e}
		return
	}
	if g.isIO(rhs, e) {
		e.vars[o] = &binding{io: true}
		return
	}
	// A local taking the address of a value, or copying a reference to it (a pointer or
	// interface), stands for that value. A copy of a plain value stands for it only
	// until something is read into the copy (see target).
	if p, ok := g.place(rhs, e); ok && !strings.HasPrefix(p, "G:") {
		u, isAddr := unparen(rhs).(*ast.UnaryExpr)
		if isAddr && u.Op == token.AND || isPlaceExpr(rhs) && isReference(g.p.info.TypeOf(rhs)) {
			e.vars[o] = &binding{place: p}
			return
		}
		if isPlaceExpr(rhs) {
			e.vars[o] = &binding{place: p, copy: true}
			return
		}
	}
	if !define {
		// An assignment of a computed value: the local no longer stands for another value.
		if b := e.lookup(o); b != nil && b.place != "" {
			e.vars[o] = &binding{place: fmt.Sprintf("L:%s@%d", o.Name(), o.Pos())}
		}
	}
}

// alias makes a field read before also stand for lhs, for code that reads into a local
// and then stores it: x.IdentityType = byte(variant).
func (g *gen) alias(lhs, rhs ast.Expr, e *env, sc *scope) {
	lp, ok1 := g.place(lhs, e)
	rp, ok2 := g.place(rhs, e)
	if !ok1 || !ok2 {
		return
	}
	for s := sc; s != nil; s = s.parent {
		if name, ok := s.places[rp]; ok {
			if name != "" {
				s.places[lp] = name
			}
			return
		}
	}
}

// lookupAssign handles x, ok := lookup(id) for lookup functions that return the
// concrete value.
func (g *gen) lookupAssign(lhs, rhs ast.Expr, e *env, sc *scope) bool {
	c, ok := unparen(rhs).(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return false
	}
	id, ok := unparen(c.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	fn, ok := g.obj(id).(*types.Func)
	if !ok {
		return false
	}
	t := g.lookupTable(fn)
	if t == nil || !t.returns {
		return false
	}
	idPlace, ok1 := g.place(c.Args[0], e)
	target, ok2 := g.place(lhs, e)
	if ok1 && ok2 {
		sc.dyn[target] = &dynamic{idPlace: idPlace, table: t}
	}
	return true
}

// isReference reports whether a copy of a value of type t still refers to the same value.
func isReference(t types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Interface, *types.Slice, *types.Map:
		return true
	}
	return false
}

// isPlaceExpr reports whether x is a selector or index chain (a copy of a field).
func isPlaceExpr(x ast.Expr) bool {
	switch unparen(x).(type) {
	case *ast.SelectorExpr:
		return true
	case *ast.StarExpr:
		return true
	}
	return false
}

func isIdent(x ast.Expr) bool { _, ok := unparen(x).(*ast.Ident); return ok }

// exprCalls runs the calls in x that can read from the IO or decide a dynamic type.
func (g *gen) exprCalls(x ast.Expr, e *env, sc *scope) {
	switch x := unparen(x).(type) {
	case *ast.CallExpr:
		g.call(x, e, sc)
	case *ast.UnaryExpr:
		g.exprCalls(x.X, e, sc)
	case *ast.BinaryExpr:
		g.exprCalls(x.X, e, sc)
		g.exprCalls(x.Y, e, sc)
	}
}

// call runs one call.
func (g *gen) call(c *ast.CallExpr, e *env, sc *scope) {
	fun := unparen(c.Fun)
	// Explicit instantiations such as SliceOfLen[T, S, A](...).
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = f.X
	case *ast.IndexListExpr:
		fun = f.X
	}
	if tv, ok := g.p.info.Types[c.Fun]; ok && tv.IsType() {
		for _, a := range c.Args {
			g.exprCalls(a, e, sc)
		}
		return
	}
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if g.isIO(f.X, e) {
			g.ioCall(f.Sel.Name, c, e, sc)
			return
		}
		if sel, ok := g.p.info.Selections[f]; ok {
			fn, ok := sel.Obj().(*types.Func)
			if !ok {
				return
			}
			g.methodCall(fn, f.X, c, e, sc)
			return
		}
		if fn, ok := g.p.info.Uses[f.Sel].(*types.Func); ok {
			g.funcCall(fn, c, e, sc)
		}
	case *ast.Ident:
		o := g.obj(f)
		if b := e.lookup(o); b != nil && b.fn != nil {
			g.inlineLit(b.fn, b.fnEnv, c.Args, e, sc)
			return
		}
		if fn, ok := o.(*types.Func); ok {
			g.funcCall(fn, c, e, sc)
		}
	case *ast.FuncLit:
		g.inlineLit(f, e, c.Args, e, sc)
	}
}

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
	if ioIgnored[name] {
		if ioErrors[name] {
			sc.errored = true
		}
		return
	}
	if len(c.Args) == 0 {
		g.diag(sc, c.Pos(), "IO method %s without arguments", name)
		return
	}
	if fieldName, ok := ioInline[name]; ok {
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
	if t, ok := ioPrimitives[name]; ok {
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
	if o, ok := overrides()[name]; ok {
		return g.fixedType("override:"+name, name, func() any { return o(g) }), true
	}
	fn := g.readerMethod(name)
	if fn == nil || !g.inIOInterface(name) {
		return nil, false
	}
	return g.namedType(fn, name, func(nsc *scope) {
		g.body(fn, map[int]*binding{-1: {io: true}, 0: {place: "$"}}, nsc, "")
	}), true
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

// body runs the body of fn with the given parameter bindings: -1 is the receiver, 0 and
// up the parameters. Parameters without a binding are fresh locals.
func (g *gen) body(fn *types.Func, binds map[int]*binding, sc *scope, ioName string) {
	decl, ok := g.p.funcs[fn.Origin()]
	if !ok {
		g.diag(sc, fn.Pos(), "no body for %s", fn.FullName())
		return
	}
	e := newEnv(nil)
	e.ioName = ioName
	if decl.Recv != nil && len(decl.Recv.List) > 0 && len(decl.Recv.List[0].Names) > 0 {
		if b, ok := binds[-1]; ok {
			if o := g.obj(decl.Recv.List[0].Names[0]); o != nil {
				e.vars[o] = b
			}
		}
	}
	i := 0
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if b, ok := binds[i]; ok {
				if o := g.obj(n); o != nil {
					e.vars[o] = b
				}
			}
			i++
		}
	}
	g.stmts(decl.Body.List, e, sc)
}

// argBinding binds an argument of a call into a function that is run.
func (g *gen) argBinding(a ast.Expr, e *env) *binding {
	if g.isIO(a, e) {
		return &binding{io: true}
	}
	if fl, ok := unparen(a).(*ast.FuncLit); ok {
		return &binding{fn: fl, fnEnv: e}
	}
	if id, ok := unparen(a).(*ast.Ident); ok {
		if b := e.lookup(g.obj(id)); b != nil && b.fn != nil {
			return b
		}
	}
	if p, ok := g.place(a, e); ok {
		return &binding{place: p}
	}
	return nil
}

// inlineFunc runs fn as part of the current container.
func (g *gen) inlineFunc(fn *types.Func, recv ast.Expr, args []ast.Expr, e *env, sc *scope, ioName string) {
	if g.inlining[fn] {
		g.diag(sc, fn.Pos(), "recursive inlined function %s", fn.Name())
		return
	}
	g.inlining[fn] = true
	defer delete(g.inlining, fn)
	binds := map[int]*binding{}
	if recv != nil {
		if b := g.argBinding(recv, e); b != nil {
			binds[-1] = b
		}
	} else {
		binds[-1] = &binding{io: true}
	}
	for i, a := range args {
		if b := g.argBinding(a, e); b != nil {
			binds[i] = b
		}
	}
	g.body(fn, binds, sc, ioName)
}

// inlineLit runs a function literal as part of the current container.
func (g *gen) inlineLit(fl *ast.FuncLit, litEnv *env, args []ast.Expr, callEnv *env, sc *scope) {
	e := newEnv(litEnv)
	i := 0
	for _, field := range fl.Type.Params.List {
		for _, n := range field.Names {
			if i < len(args) {
				if b := g.argBinding(args[i], callEnv); b != nil {
					if o := g.obj(n); o != nil {
						e.vars[o] = b
					}
				}
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
	g.stmts(fl.Body.List, e, sc)
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
	sig := fn.Type().(*types.Signature)
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
	d, _ := g.dyn(place, sc)
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

// methodType returns the named type read by a Marshal method.
func (g *gen) methodType(fn *types.Func) string {
	return g.namedType(fn, typeName(fn), func(sc *scope) {
		g.body(fn, map[int]*binding{-1: {place: "$"}, 0: {io: true}}, sc, "")
	})
}

// typeName names the type of a method's receiver, or a package-level function.
func typeName(fn *types.Func) string {
	sig := fn.Type().(*types.Signature)
	name := fn.Name()
	if sig.Recv() != nil {
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
	fn := sel.Obj().(*types.Func)
	return g.methodType(fn)
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
	sig := fn.Type().(*types.Signature)
	if sig.Params().Len() >= 2 && len(c.Args) >= 2 && g.isIO(c.Args[0], e) {
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

// funcValueType returns the type read by passing an element at place to the function
// value f (an IO method value, a function literal, a method value or a function).
func (g *gen) funcValueType(f ast.Expr, place string, e *env, sc *scope) any {
	f = unparen(f)
	switch x := f.(type) {
	case *ast.SelectorExpr:
		if g.isIO(x.X, e) {
			if t, ok := g.ioMethodType(x.Sel.Name, nil, sc, x.Pos()); ok {
				return t
			}
			g.diag(sc, x.Pos(), "IO method value %s", x.Sel.Name)
			return "void"
		}
		if sel, ok := g.p.info.Selections[x]; ok && sel.Kind() == types.MethodVal {
			fn := sel.Obj().(*types.Func)
			// A method of a context value, such as CommandEnumContext.Marshal(IO, *T).
			return g.namedType(fn, typeName(fn), func(nsc *scope) {
				g.body(fn, map[int]*binding{0: {io: true}, 1: {place: "$"}}, nsc, "")
			})
		}
		if fn, ok := g.p.info.Uses[x.Sel].(*types.Func); ok {
			return g.funcType(fn)
		}
	case *ast.Ident:
		o := g.obj(x)
		if b := e.lookup(o); b != nil && b.fn != nil {
			return g.collect(sc, func(nsc *scope) {
				g.inlineLitPlace(b.fn, b.fnEnv, place, nsc)
			})
		}
		if fn, ok := o.(*types.Func); ok {
			return g.funcType(fn)
		}
	case *ast.FuncLit:
		return g.collect(sc, func(nsc *scope) {
			g.inlineLitPlace(x, e, place, nsc)
		})
	}
	g.diag(sc, f.Pos(), "unsupported element function %T", f)
	return "void"
}

// inlineLitPlace runs a function literal whose first parameter that is not the IO
// is bound to place.
func (g *gen) inlineLitPlace(fl *ast.FuncLit, litEnv *env, place string, sc *scope) {
	e := newEnv(litEnv)
	bound := false
	for _, field := range fl.Type.Params.List {
		for _, n := range field.Names {
			o := g.obj(n)
			if o == nil {
				continue
			}
			if types.Identical(o.Type(), g.ioType) {
				e.vars[o] = &binding{io: true}
			} else if !bound {
				e.vars[o] = &binding{place: place}
				bound = true
			}
		}
	}
	g.stmts(fl.Body.List, e, sc)
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

// lookupTable recognises a lookup function: func(id T, x *I) bool, or func(id T) (I,
// bool), whose body is a switch on id with cases that set *x = &Concrete{} or return
// &Concrete{}. It returns nil for other functions.
func (g *gen) lookupTable(fn *types.Func) *lookupTable {
	if t, ok := g.lookups[fn]; ok {
		return t
	}
	g.lookups[fn] = nil
	decl, ok := g.p.funcs[fn]
	if !ok {
		return nil
	}
	sig := fn.Type().(*types.Signature)
	var iface types.Type
	returns := false
	switch {
	case sig.Params().Len() == 2 && sig.Results().Len() == 1:
		ptr, ok := sig.Params().At(1).Type().(*types.Pointer)
		if !ok {
			return nil
		}
		iface = ptr.Elem()
	case sig.Params().Len() == 1 && sig.Results().Len() == 2:
		iface, returns = sig.Results().At(0).Type(), true
	default:
		return nil
	}
	if _, ok := iface.Underlying().(*types.Interface); !ok {
		return nil
	}
	var sw *ast.SwitchStmt
	for _, s := range decl.Body.List {
		if s, ok := s.(*ast.SwitchStmt); ok {
			sw = s
		}
	}
	if sw == nil || sw.Tag == nil {
		return nil
	}
	t := &lookupTable{types: map[string]types.Type{}, returns: returns, consts: map[string]*types.Const{}}
	for _, cc := range sw.Body.List {
		cc := cc.(*ast.CaseClause)
		var concrete types.Type
		for _, s := range cc.Body {
			var x ast.Expr
			switch s := s.(type) {
			case *ast.AssignStmt:
				if len(s.Rhs) == 1 {
					x = s.Rhs[0]
				}
			case *ast.ReturnStmt:
				if len(s.Results) > 0 {
					x = s.Results[0]
				}
			}
			if u, ok := unparen(x).(*ast.UnaryExpr); ok && u.Op == token.AND {
				concrete = g.p.info.TypeOf(u.X)
			}
		}
		if concrete == nil {
			continue
		}
		for _, k := range cc.List {
			v := g.p.info.Types[k].Value
			if v == nil {
				return nil
			}
			key := constKey(v)
			t.keys = append(t.keys, key)
			t.types[key] = concrete
			t.consts[key] = g.constOf(k)
		}
	}
	if len(t.keys) == 0 {
		return nil
	}
	g.lookups[fn] = t
	return t
}

func constKey(v constant.Value) string {
	switch v.Kind() {
	case constant.Bool:
		return strconv.FormatBool(constant.BoolVal(v))
	case constant.String:
		return constant.StringVal(v)
	}
	return v.ExactString()
}

// countRef returns the ProtoDef count of an array: a number or a path to a field.
func (g *gen) countRef(x ast.Expr, e *env, sc *scope) (any, bool) {
	if v := g.p.info.Types[x].Value; v != nil {
		if n, ok := constant.Int64Val(v); ok {
			return n, true
		}
	}
	p, ok := g.place(x, e)
	if !ok {
		return nil, false
	}
	r, ok := g.ref(p, sc)
	if !ok {
		return nil, false
	}
	if strings.Contains(r, "/") {
		// ProtoDefCompiler pastes a count into JavaScript as it is, so it cannot be a path.
		g.diag(sc, x.Pos(), "array count %s is a field of an outer container", r)
		return r, true
	}
	if !unsafeCountNames[r] {
		return r, true
	}
	// The compiled array code has locals of these names, and the compiler renames fields
	// called value, size and the like: give the field a name that is pasted as is.
	for s := sc; s != nil; s = s.parent {
		if _, owns := s.places[p]; !owns {
			continue
		}
		for _, f := range s.fields {
			if f.Name == r && !f.Anon {
				f.Name = s.uniq("length")
				for q, n := range s.places {
					if n == r {
						s.places[q] = f.Name
					}
				}
				return f.Name, true
			}
		}
		break
	}
	g.diag(sc, x.Pos(), "array count field %q cannot be renamed", r)
	return r, true
}

// unsafeCountNames are field names an array count cannot refer to: locals of the code
// ProtoDefCompiler generates for an array, and names it renames in containers.
var unsafeCountNames = map[string]bool{
	"count": true, "countSize": true, "data": true, "size": true, "i": true, "elem": true,
	"value": true, "enum": true, "default": true, "offset": true, "buffer": true, "ctx": true,
}
