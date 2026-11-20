package protodef

// ioPrimitive returns the ProtoDef type of an IO method with a fixed encoding. The types
// follow the reader: Varint32 is a zigzag varint, Varuint32 a plain one, and every
// fixed-width integer and float is little-endian except BEInt32.
func ioPrimitive(name string) (any, bool) {
	t, ok := map[string]any{
		"Uint8":                   "u8",
		"Int8":                    "i8",
		"Bool":                    "bool",
		"Uint16":                  "lu16",
		"Int16":                   "li16",
		"Uint32":                  "lu32",
		"Int32":                   "li32",
		"BEInt32":                 "i32",
		"Uint64":                  "lu64",
		"Int64":                   "li64",
		"Float32":                 "lf32",
		"Float64":                 "lf64",
		"Varuint32":               "varint",
		"Varint32":                "zigzag32",
		"Varuint64":               "varint64",
		"Varint64":                "zigzag64",
		"ActorRuntimeID":          "varint64",
		"ActorRuntimeIDVarint64":  "zigzag64",
		"ActorRuntimeIDVaruint32": "varint",
		"ActorUniqueID":           "zigzag64",
		"ActorUniqueIDInt64":      "li64",
		"ActorUniqueIDUint64":     "lu64",
		"ActorUniqueIDVaruint64":  "varint64",
		"String":                  "string",
		"StringUTF":               "ShortString",
		"ByteSlice":               "ByteArray",
		"Bytes":                   "restBuffer",
		"Vec3":                    "vec3f",
		"Vec2":                    "vec2f",
		"BlockPos":                "BlockCoordinates",
		"ChunkPos":                "ChunkCoordinates",
		"SubChunkPos":             "vec3li",
		// SoundPos is a BlockPos holding the position times 8.
		"SoundPos": "BlockCoordinates",
		// ByteFloat is an angle in 1/256ths of a turn.
		"ByteFloat": "byterot",
		"RGB":       "ColourRGBf",
		"RGBA":      "lu32",
		"ARGB":      "lu32",
		"UUID":      "uuid",
		// Bitset is an unsigned LEB128 integer of any width. Every Bitset in the protocol
		// is at most 131 bits wide, which varint128 reads as long as the top bits are unset.
		"Bitset": "varint128",
	}[name]
	return t, ok
}

// isIOIgnored reports whether an IO or Reader method checks or reports but reads nothing.
func isIOIgnored(name string) bool {
	switch name {
	case "Err", "ShieldID", "UnknownEnumOption", "InvalidValue", "SliceLength", "checkRemaining", "fail", "failf":
		return true
	}
	return false
}

// isIOError reports whether an IO method marks a branch as only reached on invalid data.
func isIOError(name string) bool {
	switch name {
	case "UnknownEnumOption", "InvalidValue", "fail", "failf":
		return true
	}
	return false
}

// ioInline reports whether a composite IO method reads only part of a value (the type of
// a polymorphic value, or a number derived from it), so its fields belong to the
// container of the caller instead of a type of their own. It returns the name of the
// field the method reads.
func ioInline(name string) (string, bool) {
	switch name {
	case "EventType":
		return "event_type", true
	case "EventOrdinal":
		return "event_ordinal", true
	case "TransactionDataType":
		return "transaction_type", true
	}
	return "", false
}

// declaredNatives are the custom types a ProtoDef implementation has to provide. They
// are declared "native" in the schema as minecraft-data does.
func declaredNatives() []string {
	return []string{"varint64", "zigzag32", "zigzag64", "uuid", "byterot", "restBuffer", "encapsulated", "nbt", "lnbt"}
}

// aliases are the named composite types the primitives above refer to.
func aliases() *Map {
	m := NewMap()
	m.Set("string", []any{"pstring", map[string]any{"countType": "varint"}})
	m.Set("ShortString", []any{"pstring", map[string]any{"countType": "li16"}})
	m.Set("ByteArray", []any{"buffer", map[string]any{"countType": "varint"}})
	m.Set("vec3f", container([]*Field{{Name: "x", Type: "lf32"}, {Name: "y", Type: "lf32"}, {Name: "z", Type: "lf32"}}))
	m.Set("vec2f", container([]*Field{{Name: "x", Type: "lf32"}, {Name: "y", Type: "lf32"}}))
	m.Set("BlockCoordinates", container([]*Field{{Name: "x", Type: "zigzag32"}, {Name: "y", Type: "zigzag32"}, {Name: "z", Type: "zigzag32"}}))
	m.Set("ChunkCoordinates", container([]*Field{{Name: "x", Type: "zigzag32"}, {Name: "z", Type: "zigzag32"}}))
	m.Set("vec3li", container([]*Field{{Name: "x", Type: "li32"}, {Name: "y", Type: "li32"}, {Name: "z", Type: "li32"}}))
	m.Set("ColourRGBf", container([]*Field{{Name: "r", Type: "lf32"}, {Name: "g", Type: "lf32"}, {Name: "b", Type: "lf32"}}))
	return m
}
