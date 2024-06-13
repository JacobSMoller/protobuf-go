// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package protoavro

import (
	"fmt"

	"google.golang.org/protobuf/internal/encoding/avro"
	"google.golang.org/protobuf/internal/encoding/messageset"
	"google.golang.org/protobuf/internal/errors"
	"google.golang.org/protobuf/internal/filedesc"
	"google.golang.org/protobuf/internal/flags"
	"google.golang.org/protobuf/internal/genid"
	"google.golang.org/protobuf/internal/order"
	"google.golang.org/protobuf/internal/pragma"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const defaultIndent = "  "

// Format formats the message as a multiline string.
// This function is only intended for human consumption and ignores errors.
// Do not depend on the output being stable. Its output will change across
// different builds of your program, even when using the same version of the
// protobuf module.
func Format(m proto.Message) string {
	return MarshalOptions{Multiline: true}.Format(m)
}

// Marshal writes the given [proto.Message] in JSON format using default options.
// Do not depend on the output being stable. Its output will change across
// different builds of your program, even when using the same version of the
// protobuf module.
func Marshal(m proto.Message) ([]byte, error) {
	return MarshalOptions{}.Marshal(m)
}

// MarshalOptions is a configurable JSON format marshaler.
type MarshalOptions struct {
	pragma.NoUnkeyedLiterals

	// Multiline specifies whether the marshaler should format the output in
	// indented-form with every textual element on a new line.
	// If Indent is an empty string, then an arbitrary indent is chosen.
	Multiline bool

	// Indent specifies the set of indentation characters to use in a multiline
	// formatted output such that every entry is preceded by Indent and
	// terminated by a newline. If non-empty, then Multiline is treated as true.
	// Indent can only be composed of space or tab characters.
	Indent string

	// AllowPartial allows messages that have missing required fields to marshal
	// without returning an error. If AllowPartial is false (the default),
	// Marshal will return error if there are any missing required fields.
	AllowPartial bool

	// UseProtoNames uses proto field name instead of lowerCamelCase name in JSON
	// field names.
	UseProtoNames bool

	// UseEnumNumbers emits enum values as numbers.
	UseEnumNumbers bool

	// EmitUnpopulated specifies whether to emit unpopulated fields. It does not
	// emit unpopulated oneof fields or unpopulated extension fields.
	// The JSON value emitted for unpopulated fields are as follows:
	//  ╔═══════╤════════════════════════════╗
	//  ║ JSON  │ Protobuf field             ║
	//  ╠═══════╪════════════════════════════╣
	//  ║ false │ proto3 boolean fields      ║
	//  ║ 0     │ proto3 numeric fields      ║
	//  ║ ""    │ proto3 string/bytes fields ║
	//  ║ null  │ proto2 scalar fields       ║
	//  ║ null  │ message fields             ║
	//  ║ []    │ list fields                ║
	//  ║ {}    │ map fields                 ║
	//  ╚═══════╧════════════════════════════╝
	EmitUnpopulated bool

	// EmitDefaultValues specifies whether to emit default-valued primitive fields,
	// empty lists, and empty maps. The fields affected are as follows:
	//  ╔═══════╤════════════════════════════════════════╗
	//  ║ JSON  │ Protobuf field                         ║
	//  ╠═══════╪════════════════════════════════════════╣
	//  ║ false │ non-optional scalar boolean fields     ║
	//  ║ 0     │ non-optional scalar numeric fields     ║
	//  ║ ""    │ non-optional scalar string/byte fields ║
	//  ║ []    │ empty repeated fields                  ║
	//  ║ {}    │ empty map fields                       ║
	//  ╚═══════╧════════════════════════════════════════╝
	//
	// Behaves similarly to EmitUnpopulated, but does not emit "null"-value fields,
	// i.e. presence-sensing fields that are omitted will remain omitted to preserve
	// presence-sensing.
	// EmitUnpopulated takes precedence over EmitDefaultValues since the former generates
	// a strict superset of the latter.
	EmitDefaultValues bool

	// Resolver is used for looking up types when expanding google.protobuf.Any
	// messages. If nil, this defaults to using protoregistry.GlobalTypes.
	Resolver interface {
		protoregistry.ExtensionTypeResolver
		protoregistry.MessageTypeResolver
	}
}

// Format formats the message as a string.
// This method is only intended for human consumption and ignores errors.
// Do not depend on the output being stable. Its output will change across
// different builds of your program, even when using the same version of the
// protobuf module.
func (o MarshalOptions) Format(m proto.Message) string {
	if m == nil || !m.ProtoReflect().IsValid() {
		return "<nil>" // invalid syntax, but okay since this is for debugging
	}
	o.AllowPartial = true
	b, _ := o.Marshal(m)
	return string(b)
}

// Marshal marshals the given [proto.Message] in the JSON format using options in
// Do not depend on the output being stable. Its output will change across
// different builds of your program, even when using the same version of the
// protobuf module.
func (o MarshalOptions) Marshal(m proto.Message) ([]byte, error) {
	return o.marshal(nil, m)
}

// MarshalAppend appends the JSON format encoding of m to b,
// returning the result.
func (o MarshalOptions) MarshalAppend(b []byte, m proto.Message) ([]byte, error) {
	return o.marshal(b, m)
}

// marshal is a centralized function that all marshal operations go through.
// For profiling purposes, avoid changing the name of this function or
// introducing other code paths for marshal that do not go through this.
func (o MarshalOptions) marshal(b []byte, m proto.Message) ([]byte, error) {
	if o.Multiline && o.Indent == "" {
		o.Indent = defaultIndent
	}
	if o.Resolver == nil {
		o.Resolver = protoregistry.GlobalTypes
	}

	internalEnc := avro.NewEncoder()

	// Treat nil message interface as an empty message,
	// in which case the output in an empty JSON object.
	if m == nil {
		return append(b, '{', '}'), nil
	}

	enc := encoder{internalEnc, o}
	if err := enc.marshalMessage(m.ProtoReflect(), ""); err != nil {
		return nil, err
	}
	if o.AllowPartial {
		return enc.Bytes(), nil
	}
	return enc.Bytes(), proto.CheckInitialized(m)
}

type encoder struct {
	*avro.Encoder
	opts MarshalOptions
}

// typeFieldDesc is a synthetic field descriptor used for the "@type" field.
var typeFieldDesc = func() protoreflect.FieldDescriptor {
	var fd filedesc.Field
	fd.L0.FullName = "@type"
	fd.L0.Index = -1
	fd.L1.Cardinality = protoreflect.Optional
	fd.L1.Kind = protoreflect.StringKind
	return &fd
}()

// typeURLFieldRanger wraps a protoreflect.Message and modifies its Range method
// to additionally iterate over a synthetic field for the type URL.
type typeURLFieldRanger struct {
	order.FieldRanger
	typeURL string
}

func (m typeURLFieldRanger) Range(f func(protoreflect.FieldDescriptor, protoreflect.Value) bool) {
	if !f(typeFieldDesc, protoreflect.ValueOfString(m.typeURL)) {
		return
	}
	m.FieldRanger.Range(f)
}

// unpopulatedFieldRanger wraps a protoreflect.Message and modifies its Range
// method to additionally iterate over unpopulated fields.
type allFieldRanger struct {
	protoreflect.Message

	skipNull bool
}

func (m allFieldRanger) Range(f func(protoreflect.FieldDescriptor, protoreflect.Value) bool) {
	fds := m.Descriptor().Fields()
	fmt.Println(fds)
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		// Unsure why we are skipping fields within oneofs
		if m.Has(fd) {
			continue // ignore populated fields and fields within a oneofs
		}

		v := m.Get(fd)
		isProto2Scalar := fd.Syntax() == protoreflect.Proto2 && fd.Default().IsValid()
		isSingularMessage := fd.Cardinality() != protoreflect.Repeated && fd.Message() != nil
		if isProto2Scalar || isSingularMessage {
			if m.skipNull {
				continue
			}
			v = protoreflect.Value{} // use invalid value to emit null
		}
		// TODO: Figure out if we need to use the isSingularMessage logic here as well
		// Or does has presence work for null message types as well.
		if fd.HasPresence() {
			v = protoreflect.Value{} // pretend to have null for fields tracking presence using the optional
		}
		if !f(fd, v) {
			return
		}
	}
	m.Message.Range(f)
}

// marshalMessage marshals the fields in the given protoreflect.Message.
// If the typeURL is non-empty, then a synthetic "@type" field is injected
// containing the URL as the value.
func (e encoder) marshalMessage(m protoreflect.Message, typeURL string) error {
	if !flags.ProtoLegacy && messageset.IsMessageSet(m.Descriptor()) {
		return errors.New("no support for proto1 MessageSets")
	}

	if marshal := wellKnownTypeMarshaler(m.Descriptor().FullName()); marshal != nil {
		return marshal(e, m)
	}

	var fields order.FieldRanger
	fields = allFieldRanger{Message: m, skipNull: false}
	if typeURL != "" {
		fields = typeURLFieldRanger{fields, typeURL}
	}

	var err error
	order.RangeFields(fields, order.IndexNameFieldOrder, func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if err = e.marshalValue(v, fd); err != nil {
			return false
		}
		return true
	})
	return err
}

// marshalValue marshals the given protoreflect.Value.
func (e encoder) marshalValue(val protoreflect.Value, fd protoreflect.FieldDescriptor) error {
	switch {
	case fd.IsList():
		return e.marshalList(val.List(), fd)
	case fd.IsMap():
		return e.marshalMap(val.Map(), fd)
	default:
		return e.marshalSingular(val, fd)
	}
}

// marshalSingular marshals the given non-repeated field value. This includes
// all scalar types, enums, messages, and groups.
func (e encoder) marshalSingular(val protoreflect.Value, fd protoreflect.FieldDescriptor) error {
	fmt.Println(fd.FullName(), val)
	if !val.IsValid() {
		e.WriteNull()
		return nil
	}

	switch kind := fd.Kind(); kind {
	case protoreflect.BoolKind:
		e.WriteBool(val.Bool())

	case protoreflect.StringKind:
		e.WriteString(val.String())

	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		e.WriteInt(val.Int())

	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		e.WriteLong(int64(val.Uint()))

	// TODO handle Uint64Kind, and fixed64Kind indivudually and represent them as decimals
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind:
		e.WriteLong(val.Int())

	case protoreflect.FloatKind:
		e.WriteFloat(float32(val.Float()))

	case protoreflect.DoubleKind:
		e.WriteDouble(val.Float())

	case protoreflect.BytesKind:
		e.WriteBytes(val.Bytes())

	case protoreflect.EnumKind:
		if fd.Enum().FullName() == genid.NullValue_enum_fullname {
			e.WriteNull()
		} else {
			e.WriteInt(int64(val.Enum()))
		}

	case protoreflect.MessageKind, protoreflect.GroupKind:
		e.WriteRecordUnionIndex()
		if err := e.marshalMessage(val.Message(), ""); err != nil {
			return err
		}

	default:
		panic(fmt.Sprintf("%v has unknown kind: %v", fd.FullName(), kind))
	}
	return nil
}

// TODO implement block size defaulting to 100 for the 2 functions below
// marshalList marshals the given protoreflect.List.
func (e encoder) marshalList(list protoreflect.List, fd protoreflect.FieldDescriptor) error {
	e.StartBlock(int64(list.Len()))
	for i := 0; i < list.Len(); i++ {
		item := list.Get(i)
		if err := e.marshalSingular(item, fd); err != nil {
			return err
		}
	}
	e.EndBlock()
	return nil
}

// marshalMap marshals given protoreflect.Map.
func (e encoder) marshalMap(mmap protoreflect.Map, fd protoreflect.FieldDescriptor) error {
	var err error
	e.StartBlock(int64(mmap.Len()))
	order.RangeEntries(mmap, order.AnyKeyOrder, func(k protoreflect.MapKey, v protoreflect.Value) bool {
		e.WriteName(k.String())
		if err = e.marshalSingular(v, fd.MapValue()); err != nil {
			return false
		}
		return true
	})
	e.EndBlock()
	return err
}