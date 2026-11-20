package protodef

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/helix-bedrock/protocol"
	"github.com/helix-bedrock/protocol/packet"
)

const testShieldID = 355

var (
	schemaOnce sync.Once
	schemaVal  *Schema
	schemaJSON map[string]any
	schemaErr  error
)

// testSchema generates the schema once for all tests and reads it back from JSON, so
// the tests check the JSON as written.
func testSchema(t *testing.T) (*Schema, map[string]any) {
	t.Helper()
	schemaOnce.Do(func() {
		schemaVal, schemaErr = Generate(filepath.Join("..", ".."))
		if schemaErr != nil {
			return
		}
		var data []byte
		if data, schemaErr = json.Marshal(schemaVal); schemaErr != nil {
			return
		}
		var doc struct {
			Types map[string]any `json:"types"`
		}
		schemaErr = json.Unmarshal(data, &doc)
		schemaJSON = doc.Types
	})
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	return schemaVal, schemaJSON
}

func TestGenerateHasNoDiagnostics(t *testing.T) {
	s, _ := testSchema(t)
	for _, d := range s.Diagnostics {
		t.Error(d)
	}
}

// TestSchemaRefsResolve checks that every type a schema names is defined.
func TestSchemaRefsResolve(t *testing.T) {
	_, types := testSchema(t)
	builtin := map[string]bool{
		"void": true, "bool": true, "u8": true, "i8": true, "li16": true, "lu16": true, "li32": true, "lu32": true,
		"i32": true, "li64": true, "lu64": true, "lf32": true, "lf64": true, "varint": true, "varint128": true,
		"container": true, "array": true, "option": true, "switch": true, "mapper": true, "pstring": true, "buffer": true,
	}
	var missing []string
	var walk func(where string, t any)
	walk = func(where string, t any) {
		switch t := t.(type) {
		case string:
			if !builtin[t] {
				if _, ok := types[t]; !ok {
					missing = append(missing, where+": "+t)
				}
			}
		case []any:
			kind, arg := typeKind(t)
			switch kind {
			case "container":
				for _, f := range arg.([]any) {
					walk(where, f.(map[string]any)["type"])
				}
			case "array", "mapper":
				walk(where, arg.(map[string]any)["type"])
			case "option":
				walk(where, arg)
			case "switch":
				opts := arg.(map[string]any)
				for _, c := range opts["fields"].(map[string]any) {
					walk(where, c)
				}
				if d, ok := opts["default"]; ok {
					walk(where, d)
				}
			case "encapsulated":
				opts := arg.(map[string]any)
				walk(where, opts["type"])
				walk(where, opts["lengthType"])
			}
		}
	}
	for name, def := range types {
		if def != "native" {
			walk(name, def)
		}
	}
	for _, m := range missing {
		t.Errorf("undefined type %s", m)
	}
}

// TestRoundTrip checks the schema against the Go code for every packet, in both
// directions:
//
//  1. a random value of the packet's schema type is encoded, and the Reader must read
//     it without error and without bytes left over;
//  2. the packet the Reader read is written again by the Writer, and the schema must
//     decode those bytes, again with nothing left over;
//  3. the schema must decode the same values from both, except for the fields listed in
//     rewrittenFields, which the Writer derives from other values. This catches a field
//     read with the wrong type of the same size, such as li32 for lf32.
//
// Values are random but steered: a switch picks one of its cases, and a count matches
// its array, so every branch of the schema is reached.
func TestRoundTrip(t *testing.T) {
	s, types := testSchema(t)
	in := &interp{types: types, vars: map[string]any{"ShieldItemID": int64(testShieldID)}}
	pools := []packet.Pool{packet.NewServerPool(), packet.NewClientPool()}
	const samples = 400
	taken := map[string]map[string]bool{}
	defer func() { reportCoverage(t, types, taken) }()
	for _, pk := range s.Packets {
		t.Run(pk.Name, func(t *testing.T) {
			var newPacket func() packet.Packet
			for _, p := range pools {
				if f, ok := p[pk.ID]; ok {
					newPacket = f
				}
			}
			if newPacket == nil {
				t.Fatalf("packet %d not in any pool", pk.ID)
			}
			typ := "packet_" + pk.Name
			rng := rand.New(rand.NewPCG(uint64(pk.ID), 1))
			accepted, generated := 0, 0
			var firstReject error
			for range samples {
				g := &generator{interp: in, rng: rng, fixed: map[*obj]map[string]bool{}, strings: s.StringValues}
				v := g.gen(typ, nil)
				if g.conflict {
					continue
				}
				enc := &encoder{interp: in}
				if err := enc.write(typ, v, nil); err != nil {
					t.Fatalf("encode generated value: %v", err)
				}
				generated++
				data := enc.buf.Bytes()

				// 1: schema -> Reader.
				buf := bytes.NewBuffer(data)
				r := protocol.NewReader(buf, testShieldID, true)
				read := newPacket()
				read.Marshal(r)
				if err := r.Err(); err != nil {
					if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("Reader ran out of data reading what the schema wrote: %v\n%s", err, hex.Dump(data))
					}
					if firstReject == nil {
						firstReject = err
					}
					continue
				}
				if buf.Len() != 0 {
					t.Fatalf("Reader left %d of %d bytes the schema wrote\n%s", buf.Len(), len(data), hex.Dump(data))
				}
				accepted++
				// Count the cases this sample reaches; the schema wrote it, so it decodes.
				cov := &decoder{interp: in, buf: data, taken: taken}
				genVal, err := cov.read(typ, nil)
				if err != nil || cov.off != len(data) {
					t.Fatalf("schema cannot decode what it wrote (%v)\n%s", err, hex.Dump(data))
				}
				// The values the schema decodes are the ones the Reader stored.
				if o, ok := genVal.(*obj); ok {
					cmp := &comparer{in: in, goFields: s.goFields}
					cmp.object(typ, o, reflect.ValueOf(read), pk.Name)
					if len(cmp.errs) > 0 {
						t.Fatalf("the schema decodes other values than the Reader:\n%s\n%s", strings.Join(cmp.errs, "\n"), hex.Dump(data))
					}
				}

				// 2: Writer -> schema.
				var out bytes.Buffer
				w := protocol.NewWriter(&out, testShieldID)
				read.Marshal(w)
				if err := w.Err(); err != nil {
					t.Fatalf("Writer failed on a packet the Reader read: %v", err)
				}
				written := out.Bytes()
				// 3: the Writer wrote back the values it read, except the ones it derives.
				if !bytes.Equal(written, data) {
					genVal, _ := (&decoder{interp: in, buf: data}).read(typ, nil)
					wroteVal, _ := (&decoder{interp: in, buf: written}).read(typ, nil)
					diffs := map[string]bool{}
					diffValues(genVal, wroteVal, "", diffs)
					for _, path := range keysOf(boolMap(diffs)) {
						if !rewritten(pk.Name + path) {
							t.Fatalf("the Writer wrote %s back with another value: the schema may read it with the wrong type\ngenerated:\n%swritten:\n%s", pk.Name+path, hex.Dump(data), hex.Dump(written))
						}
					}
				}
				d := &decoder{interp: in, buf: written, taken: taken}
				if _, err := d.read(typ, nil); err != nil {
					t.Fatalf("schema cannot decode what the Writer wrote: %v\nwritten:\n%sgenerated:\n%s", err, hex.Dump(written), hex.Dump(data))
				}
				if d.off != len(written) {
					t.Fatalf("schema left %d of %d bytes the Writer wrote\n%s", len(written)-d.off, len(written), hex.Dump(written))
				}
			}
			if testing.Verbose() {
				t.Logf("%d of %d generated samples accepted; first rejection: %v", accepted, generated, firstReject)
			}
			if accepted == 0 {
				t.Fatalf("the Reader accepted none of %d generated samples; first error: %v", generated, firstReject)
			}
		})
	}
}

// reportCoverage logs the switch cases that no sample the Reader accepted reached.
func reportCoverage(t *testing.T, types map[string]any, taken map[string]map[string]bool) {
	total, hit := 0, 0
	var missed []string
	var walk func(where string, t any)
	walk = func(where string, t any) {
		kind, arg := typeKind(t)
		switch kind {
		case "container":
			for _, f := range arg.([]any) {
				fm := f.(map[string]any)
				walk(where+"."+fieldLabel(fm), fm["type"])
			}
		case "array", "mapper":
			walk(where, arg.(map[string]any)["type"])
		case "option":
			walk(where, arg)
		case "switch":
			opts := arg.(map[string]any)
			site := fmt.Sprintf("%p", opts)
			keys := keysOf(opts["fields"].(map[string]any))
			if _, ok := opts["default"]; ok {
				keys = append(keys, "default")
			}
			for _, k := range keys {
				total++
				if taken[site][k] {
					hit++
				} else {
					missed = append(missed, where+"["+k+"]")
				}
			}
			for _, c := range opts["fields"].(map[string]any) {
				walk(where, c)
			}
			if d, ok := opts["default"]; ok {
				walk(where, d)
			}
		}
	}
	for _, name := range keysOf(types) {
		// mcpe_packet is not decoded: each packet type is tested on its own.
		if name != "mcpe_packet" {
			walk(name, types[name])
		}
	}
	sort.Strings(missed)
	t.Logf("switch cases reached: %d of %d", hit, total)
	for _, m := range missed {
		t.Logf("not reached: %s", m)
	}
}

// TestNamesCompile checks the names node-protodef's ProtoDefCompiler turns into
// JavaScript identifiers: field names, and array and string counts, which it pastes
// into the generated code as they are.
func TestNamesCompile(t *testing.T) {
	_, types := testSchema(t)
	ident := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	var walk func(where string, typ any)
	walk = func(where string, typ any) {
		kind, arg := typeKind(typ)
		switch kind {
		case "container":
			for _, f := range arg.([]any) {
				fm := f.(map[string]any)
				if name, ok := fm["name"].(string); ok {
					// A field called buffer or ctx would hide the compiled reader's parameters.
					if !ident.MatchString(name) || name == "buffer" || name == "ctx" {
						t.Errorf("%s: field name %q", where, name)
					}
				}
				walk(where+"."+fieldLabel(fm), fm["type"])
			}
		case "array", "pstring", "buffer":
			opts, _ := arg.(map[string]any)
			if c, ok := opts["count"].(string); ok && (!ident.MatchString(c) || unsafeCountNames[c]) {
				t.Errorf("%s: count %q", where, c)
			}
			if kind == "array" {
				walk(where, opts["type"])
			}
		case "mapper":
			walk(where, arg.(map[string]any)["type"])
		case "option":
			walk(where, arg)
		case "switch":
			opts := arg.(map[string]any)
			for _, c := range opts["fields"].(map[string]any) {
				walk(where, c)
			}
			if d, ok := opts["default"]; ok {
				walk(where, d)
			}
		}
	}
	for _, name := range keysOf(types) {
		walk(name, types[name])
	}
}

func boolMap(m map[string]bool) map[string]any {
	out := map[string]any{}
	for k := range m {
		out[k] = nil
	}
	return out
}

// rewrittenFields are the fields (as packet name and path, [] for array elements) that
// the Writer does not write back as read, with the reason found in the Go code.
var rewrittenFields = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile(`\.extra(\.|$)`), "item user data: the Writer sends none for air and picks the NBT marker itself (writer.go itemUserData)"},
	{regexp.MustCompile(`\.(has_stack_id|stack_id)$`), "ItemInstance: has_stack_id is StackNetworkID != 0 (writer.go ItemInstance)"},
	{regexp.MustCompile(`\.entity_metadata\.`), "EntityMetadata is a map: the Writer sends it in key order, once per key"},
	{regexp.MustCompile(`^resource_pack_client_response\.name$`), "the response name is written from the response type"},
	{regexp.MustCompile(`^text\.category_type$`), "the category is derived from the text type"},
	{regexp.MustCompile(`^inventory_transaction\.(has_legacy|legacy_set_item_slots)$`), "has_legacy is derived from LegacyRequestID"},
	{regexp.MustCompile(`\.material_reducers\[\]\.mix$`), "the reader splits mix into ID and metadata lossily (reader.go MaterialReducer)"},
	{regexp.MustCompile(`\.unlock_requirement\.(present|ingredients)`), "present is derived from the unlock context (recipe.go RecipeUnlockRequirement)"},
	{regexp.MustCompile(`\.aux$`), "item descriptors: the Writer sends 32767 for an empty descriptor"},
	{regexp.MustCompile(`^player_list\.entries\[\]\.legacy_action$`), "the legacy action byte is written from the action and not read back"},
	{regexp.MustCompile(`\.skin\.trusted$`), "Trusted is a bool sent as a string"},
	{regexp.MustCompile(`^event\.event_ordinal$`), "the ordinal is derived from the event type"},
	{regexp.MustCompile(`^(play_sound|clientbound_play_audio_content)\.(sound\.)?position\.[xyz]$`), "SoundPos is a float position times 8 in a BlockPos"},
	{regexp.MustCompile(`^set_score\.entries\[\]\.type_name$`), "the type name is written from the identity type"},
	{regexp.MustCompile(`\.legacy_(id|variant)$`), "the legacy IDs are written from the action or variant"},
	{regexp.MustCompile(`^item_stack_response\.responses\[\]\.container_info$`), "the optional is present when there are slots"},
	{regexp.MustCompile(`^request_ability\.value\.(bool_val|float_val)$`), "the value not used by the type is sent as zero"},
	{regexp.MustCompile(`\.colour\.[rgb]$`), "RGB floats are kept as bytes"},
	{regexp.MustCompile(`^player_update_entity_overrides\.name$`), "the type name is written from the type"},
	{regexp.MustCompile(`^player_location\.type$`), "the type is written twice and read back from the second"},
	{regexp.MustCompile(`^clientbound_update_sound_data\.update\.index$`), "the index is written from the type"},
}

func rewritten(path string) bool {
	for _, f := range rewrittenFields {
		if f.pattern.MatchString(path) {
			return true
		}
	}
	return false
}
