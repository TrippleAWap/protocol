package protodef

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// Packet is a packet found in the packet pool.
type Packet struct {
	ID uint32
	// Name is the snake_case name used in mcpe_packet; its type is "packet_" + Name.
	Name string
	// GoName is the name of the Go type.
	GoName string
	// FromServer and FromClient say which pools the packet is in.
	FromServer, FromClient bool
}

// Schema is a generated ProtoDef schema.
type Schema struct {
	// Types holds every type in the order written to protocol.json.
	Types   *Map
	Packets []Packet
	// StringValues maps a type name, then a field name, to the only values the Reader
	// accepts for that string field. ProtoDef cannot say this, so it is not in the JSON.
	StringValues map[string]map[string][]string
	// goFields maps a type name, then a field name, to the paths of the Go struct fields
	// the Reader reads it into (one per switch case), for fields read straight into one.
	// A path starting with ~ is a field of another integer type (IntegerFunc).
	goFields map[string]map[string][]string
	// Diagnostics lists the code the generator could not turn into ProtoDef. A schema
	// with diagnostics is incomplete where they point.
	Diagnostics []string
}

// MarshalJSON writes the schema in the layout of minecraft-data's protocol.json.
func (s *Schema) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"types": s.Types})
}

// Generate builds the schema from the protocol module in dir.
func Generate(dir string) (*Schema, error) {
	p, err := load(dir)
	if err != nil {
		return nil, err
	}
	ioObj, ok1 := p.root.Scope().Lookup("IO").(*types.TypeName)
	readerObj, ok2 := p.root.Scope().Lookup("Reader").(*types.TypeName)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("protodef: protocol.IO or protocol.Reader not found")
	}
	g := &gen{
		p:        p,
		ioType:   ioObj.Type(),
		reader:   types.NewPointer(readerObj.Type()),
		named:    NewMap(),
		names:    map[any]string{},
		owners:   map[string]any{},
		lookups:  map[*types.Func]*lookupTable{},
		inlining: map[*types.Func]bool{},
		strings:  map[string]map[string][]string{},
		consts:   map[*Field]map[*types.Const]bool{},
		goFields: map[string]map[string]*Field{},
	}
	for _, n := range declaredNatives {
		g.owners[n] = n
	}
	al := aliases()
	for _, n := range al.Keys() {
		g.owners[n] = n
	}

	packets, err := g.packets()
	if err != nil {
		return nil, err
	}
	ids := NewMap()
	params := NewMap()
	packetTypes := NewMap()
	for _, pk := range packets {
		obj, _ := p.packet.Scope().Lookup(pk.GoName).(*types.TypeName)
		if obj == nil {
			return nil, fmt.Errorf("protodef: packet type %s not found", pk.GoName)
		}
		sel := types.NewMethodSet(types.NewPointer(obj.Type())).Lookup(p.packet, "Marshal")
		if sel == nil {
			return nil, fmt.Errorf("protodef: packet %s has no Marshal method", pk.GoName)
		}
		fn := sel.Obj().(*types.Func)
		name := g.namedType(fn, "packet_"+pk.Name, func(sc *scope) {
			g.body(fn, map[int]*binding{-1: {place: "$"}, 0: {io: true}}, sc, "")
		})
		ids.Set(fmt.Sprint(pk.ID), pk.Name)
		params.Set(pk.Name, name)
		packetTypes.Set(name, nil)
	}

	g.applyMappers()

	out := NewMap()
	for _, n := range declaredNatives {
		out.Set(n, "native")
	}
	for _, n := range al.Keys() {
		v, _ := al.Get(n)
		out.Set(n, v)
	}
	var shared []string
	for _, n := range g.named.Keys() {
		if _, isPacket := packetTypes.Get(n); !isPacket {
			shared = append(shared, n)
		}
	}
	sort.Strings(shared)
	for _, n := range shared {
		v, _ := g.named.Get(n)
		out.Set(n, v)
	}
	out.Set("mcpe_packet", container([]*Field{
		{Name: "name", Type: mapper("varint", ids)},
		{Name: "params", Type: switchOf("name", params, nil)},
	}))
	for _, n := range packetTypes.Keys() {
		v, _ := g.named.Get(n)
		out.Set(n, v)
	}

	diags := append([]string(nil), g.diags...)
	for _, sc := range g.pending {
		diags = append(diags, sc.diags...)
	}
	sort.Strings(diags)
	diags = dedupe(diags)
	goFields := map[string]map[string][]string{}
	for typ, fields := range g.goFields {
		goFields[typ] = map[string][]string{}
		for path, f := range fields {
			goFields[typ][f.Name] = append(goFields[typ][f.Name], path)
		}
	}
	return &Schema{Types: out, Packets: packets, StringValues: g.strings, Diagnostics: diags, goFields: goFields}, nil
}

func dedupe(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// packets reads the packet pool from the init function of the packet package: every
// map literal of IDs to func() Packet { return &T{} }.
func (g *gen) packets() ([]Packet, error) {
	byID := map[uint32]*Packet{}
	for _, f := range g.p.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "init" || fd.Recv != nil || fd.Body == nil {
				continue
			}
			if g.p.info.Defs[fd.Name] == nil || g.p.info.Defs[fd.Name].Pkg() != g.p.packet {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				lit, ok := as.Rhs[0].(*ast.CompositeLit)
				if !ok {
					return true
				}
				varName := ""
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					varName = strings.ToLower(id.Name)
				}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					v := g.p.info.Types[kv.Key].Value
					if v == nil {
						continue
					}
					id64, ok := constant.Uint64Val(v)
					if !ok {
						continue
					}
					goName := packetTypeName(kv.Value)
					if goName == "" {
						continue
					}
					id := uint32(id64)
					pk := byID[id]
					if pk == nil {
						pk = &Packet{ID: id, Name: snake(goName), GoName: goName}
						byID[id] = pk
					}
					pk.FromServer = pk.FromServer || strings.Contains(varName, "server")
					pk.FromClient = pk.FromClient || strings.Contains(varName, "client")
				}
				return true
			})
		}
	}
	if len(byID) == 0 {
		return nil, fmt.Errorf("protodef: no packets found in the packet pool")
	}
	out := make([]Packet, 0, len(byID))
	for _, pk := range byID {
		out = append(out, *pk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// packetTypeName returns T from func() Packet { return &T{} }.
func packetTypeName(x ast.Expr) string {
	fl, ok := x.(*ast.FuncLit)
	if !ok || len(fl.Body.List) != 1 {
		return ""
	}
	ret, ok := fl.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return ""
	}
	u, ok := ret.Results[0].(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return ""
	}
	cl, ok := u.X.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	if id, ok := cl.Type.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
