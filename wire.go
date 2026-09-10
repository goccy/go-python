package python

// The host half of the typed value protocol (see python-wasm's py.h for the
// authoritative wire description): little-endian binary nodes for values,
// result envelopes for operations. Nothing is stringified in transit —
// immutable built-ins cross by value with their kind (bytes raw, str as
// UTF-8 with surrogatepass), every other object crosses by handle.

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/goccy/go-python/internal"
)

// Node tags.
const (
	wireNone     = 0
	wireBool     = 1
	wireInt      = 2
	wireBigInt   = 3
	wireFloat    = 4
	wireComplex  = 5
	wireStr      = 6
	wireBytes    = 7
	wireObject   = 8
	wireHostFunc = 9
)

// Object node kinds (what the handle's object is).
const (
	kindObject    = 0
	kindList      = 1
	kindTuple     = 2
	kindDict      = 3
	kindSet       = 4
	kindFrozenSet = 5
	kindFunction  = 6
	kindClass     = 7
	kindModule    = 8
)

// Object node flags.
const flagCallable = 1

// Envelope statuses.
const (
	wireOK     = 0
	wireRaised = 1
	wireExit   = 2
)

func appendU16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func appendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func appendU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func appendF64(b []byte, v float64) []byte {
	return binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
}
func appendLenBytes(b []byte, s []byte) []byte {
	b = appendU32(b, uint32(len(s)))
	return append(b, s...)
}

// encodeValue appends v's node. A nil Value is None.
func (p *Python) encodeValue(b []byte, v Value) ([]byte, error) {
	switch x := v.(type) {
	case nil, NoneValue:
		return append(b, wireNone), nil
	case BoolValue:
		b = append(b, wireBool)
		if x.b {
			return append(b, 1), nil
		}
		return append(b, 0), nil
	case IntValue:
		if x.big == nil {
			b = append(b, wireInt)
			return appendU64(b, uint64(x.i)), nil
		}
		// Magnitude little-endian: big.Int.Bytes is big-endian.
		mag := x.big.Bytes()
		for i, j := 0, len(mag)-1; i < j; i, j = i+1, j-1 {
			mag[i], mag[j] = mag[j], mag[i]
		}
		b = append(b, wireBigInt)
		if x.big.Sign() < 0 {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		return appendLenBytes(b, mag), nil
	case FloatValue:
		b = append(b, wireFloat)
		return appendF64(b, x.f), nil
	case ComplexValue:
		b = append(b, wireComplex)
		b = appendF64(b, real(x.c))
		return appendF64(b, imag(x.c)), nil
	case StrValue:
		b = append(b, wireStr)
		return appendLenBytes(b, []byte(x.s)), nil
	case BytesValue:
		b = append(b, wireBytes)
		return appendLenBytes(b, x.b), nil
	case ObjectValue:
		return p.encodeHandle(b, x.handle())
	default:
		return nil, fmt.Errorf("python: cannot encode %T", v)
	}
}

// encodeHandle appends an object node for a handle this instance owns.
func (p *Python) encodeHandle(b []byte, h *objectHandle) ([]byte, error) {
	if h == nil || h.released.Load() {
		return nil, fmt.Errorf("python: object handle has been released")
	}
	if h.p != p {
		return nil, fmt.Errorf("python: value belongs to a different Python instance")
	}
	b = append(b, wireObject)
	b = appendU64(b, h.id)
	b = append(b, h.kind)
	var flags byte
	if h.callable {
		flags |= flagCallable
	}
	b = append(b, flags)
	return appendU16(b, 0), nil // type name travels guest->host only
}

// encodeSingle encodes one node.
func (p *Python) encodeSingle(v Value) ([]byte, error) {
	return p.encodeValue(nil, v)
}

// encodeList encodes a node list ([u32 count] nodes).
func (p *Python) encodeList(vals []Value) ([]byte, error) {
	b := appendU32(nil, uint32(len(vals)))
	var err error
	for i, v := range vals {
		b, err = p.encodeValue(b, v)
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
	}
	return b, nil
}

// encodeDictItems encodes key/value pairs as a flat node list of 2n nodes.
func (p *Python) encodeDictItems(items []DictItem) ([]byte, error) {
	b := appendU32(nil, uint32(2*len(items)))
	var err error
	for i, it := range items {
		if b, err = p.encodeValue(b, it.Key); err != nil {
			return nil, fmt.Errorf("item %d key: %w", i, err)
		}
		if b, err = p.encodeValue(b, it.Value); err != nil {
			return nil, fmt.Errorf("item %d value: %w", i, err)
		}
	}
	return b, nil
}

// encodeCallArgs splits a Call argument list into the positional node list
// and the kwargs list.
func (p *Python) encodeCallArgs(args []Arg) (pos, kw []byte, err error) {
	var positional []Value
	var keywords []kwarg
	for i, a := range args {
		switch x := a.(type) {
		case kwarg:
			keywords = append(keywords, x)
		case Value:
			positional = append(positional, x)
		case nil:
			positional = append(positional, None())
		default:
			return nil, nil, fmt.Errorf("argument %d: cannot encode %T", i, a)
		}
	}
	pos, err = p.encodeList(positional)
	if err != nil {
		return nil, nil, err
	}
	kw = appendU32(nil, uint32(len(keywords)))
	for _, k := range keywords {
		kw = appendLenBytes(kw, []byte(k.name))
		kw, err = p.encodeValue(kw, k.v)
		if err != nil {
			return nil, nil, fmt.Errorf("keyword argument %s: %w", k.name, err)
		}
	}
	return pos, kw, nil
}

// nodeReader walks a response buffer with bounds checking; any overrun
// flips fail so decoding is total.
type nodeReader struct {
	b    []byte
	pos  int
	fail bool
}

func (r *nodeReader) need(n int) bool {
	if r.fail || len(r.b)-r.pos < n {
		r.fail = true
		return false
	}
	return true
}

func (r *nodeReader) u8() byte {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *nodeReader) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}

func (r *nodeReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

func (r *nodeReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v
}

func (r *nodeReader) f64() float64 { return math.Float64frombits(r.u64()) }

// bytes returns the next n bytes (a view into the buffer).
func (r *nodeReader) bytes(n int) []byte {
	if n < 0 || !r.need(n) {
		return nil
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v
}

// lenBytes reads a u32 length followed by that many bytes.
func (r *nodeReader) lenBytes() []byte {
	n := r.u32()
	return r.bytes(int(n))
}

func (r *nodeReader) lenString() string { return string(r.lenBytes()) }

// decodeNode decodes one node into a Value. Object nodes become the
// concrete handle type their kind byte names.
func (p *Python) decodeNode(r *nodeReader) (Value, error) {
	tag := r.u8()
	if r.fail {
		return nil, fmt.Errorf("python: malformed value node")
	}
	switch tag {
	case wireNone:
		return None(), nil
	case wireBool:
		return BoolValue{b: r.u8() != 0}, nil
	case wireInt:
		return IntValue{i: int64(r.u64())}, nil
	case wireBigInt:
		neg := r.u8() != 0
		mag := r.lenBytes()
		if r.fail {
			return nil, fmt.Errorf("python: malformed bigint node")
		}
		be := make([]byte, len(mag))
		for i, c := range mag {
			be[len(mag)-1-i] = c
		}
		v := new(big.Int).SetBytes(be)
		if neg {
			v.Neg(v)
		}
		return intFromBig(v), nil
	case wireFloat:
		return FloatValue{f: r.f64()}, nil
	case wireComplex:
		re := r.f64()
		im := r.f64()
		return ComplexValue{c: complex(re, im)}, nil
	case wireStr:
		raw := r.lenBytes()
		if r.fail {
			return nil, fmt.Errorf("python: malformed str node")
		}
		return StrValue{s: string(raw)}, nil
	case wireBytes:
		raw := r.lenBytes()
		if r.fail {
			return nil, fmt.Errorf("python: malformed bytes node")
		}
		b := make([]byte, len(raw))
		copy(b, raw)
		return BytesValue{b: b}, nil
	case wireObject:
		id := r.u64()
		kind := r.u8()
		flags := r.u8()
		typeName := string(r.bytes(int(r.u16())))
		if r.fail {
			return nil, fmt.Errorf("python: malformed object node")
		}
		h := newObjectHandle(p, id, kind, flags&flagCallable != 0, typeName)
		return objectFor(h), nil
	default:
		return nil, fmt.Errorf("python: unknown value node tag %d", tag)
	}
}

// objectFor wraps a handle in the concrete type its kind names.
func objectFor(h *objectHandle) Value {
	o := object{h: h}
	switch h.kind {
	case kindList:
		return ListValue{o}
	case kindTuple:
		return TupleValue{o}
	case kindDict:
		return DictValue{o}
	case kindSet:
		return SetValue{o}
	case kindFrozenSet:
		return FrozenSetValue{o}
	case kindFunction:
		return FunctionValue{o}
	case kindClass:
		return ClassValue{o}
	case kindModule:
		return ModuleValue{o}
	default:
		return o
	}
}

// decodeEnvelope consumes the status byte; a raised envelope becomes a
// *PythonError (or the cancellation when the interrupt watchdog fired), a
// caught SystemExit becomes the error ExitCode recognises. The reader is
// positioned at the ok payload on nil error.
func (p *Python) decodeEnvelope(ctx context.Context, r *nodeReader, interrupted bool) error {
	switch r.u8() {
	case wireOK:
		return nil
	case wireRaised:
		pe, err := p.decodeRaised(r)
		if err != nil {
			return err
		}
		if interrupted {
			return ctx.Err()
		}
		return pe
	case wireExit:
		return &internal.ExitError{Code: int(int32(r.u32()))}
	default:
		return fmt.Errorf("python: malformed result envelope")
	}
}

// decodeRaised reads the raised payload: type, message, traceback, and the
// exception instance (a handle, or none for a bridge-level failure).
func (p *Python) decodeRaised(r *nodeReader) (*PythonError, error) {
	pe := &PythonError{
		Type:      r.lenString(),
		Message:   r.lenString(),
		Traceback: r.lenString(),
	}
	if r.fail {
		return nil, fmt.Errorf("python: malformed raised envelope")
	}
	v, err := p.decodeNode(r)
	if err != nil {
		return nil, err
	}
	if ov, ok := v.(ObjectValue); ok {
		pe.Value = ov
	}
	return pe, nil
}

// decodeNodeResult decodes an ok envelope carrying one node.
func (p *Python) decodeNodeResult(ctx context.Context, resp []byte, interrupted bool) (Value, error) {
	r := &nodeReader{b: resp}
	if err := p.decodeEnvelope(ctx, r, interrupted); err != nil {
		return nil, err
	}
	return p.decodeNode(r)
}

// decodeListResult decodes an ok envelope carrying a node list.
func (p *Python) decodeListResult(ctx context.Context, resp []byte, interrupted bool) ([]Value, error) {
	r := &nodeReader{b: resp}
	if err := p.decodeEnvelope(ctx, r, interrupted); err != nil {
		return nil, err
	}
	count := int(r.u32())
	if r.fail {
		return nil, fmt.Errorf("python: malformed node list")
	}
	out := make([]Value, 0, count)
	for i := 0; i < count; i++ {
		v, err := p.decodeNode(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// decodeI64Result decodes an ok envelope carrying one i64.
func (p *Python) decodeI64Result(ctx context.Context, resp []byte, interrupted bool) (int64, error) {
	r := &nodeReader{b: resp}
	if err := p.decodeEnvelope(ctx, r, interrupted); err != nil {
		return 0, err
	}
	v := int64(r.u64())
	if r.fail {
		return 0, fmt.Errorf("python: malformed result envelope")
	}
	return v, nil
}

// decodeBoolResult decodes an ok envelope carrying one u8 flag.
func (p *Python) decodeBoolResult(ctx context.Context, resp []byte, interrupted bool) (bool, error) {
	r := &nodeReader{b: resp}
	if err := p.decodeEnvelope(ctx, r, interrupted); err != nil {
		return false, err
	}
	v := r.u8() != 0
	if r.fail {
		return false, fmt.Errorf("python: malformed result envelope")
	}
	return v, nil
}

// decodeTextResult decodes an ok envelope carrying u32 len + bytes.
func (p *Python) decodeTextResult(ctx context.Context, resp []byte, interrupted bool) (string, error) {
	r := &nodeReader{b: resp}
	if err := p.decodeEnvelope(ctx, r, interrupted); err != nil {
		return "", err
	}
	s := r.lenString()
	if r.fail {
		return "", fmt.Errorf("python: malformed result envelope")
	}
	return s, nil
}

// decodeEmptyResult decodes an ok envelope with no payload.
func (p *Python) decodeEmptyResult(ctx context.Context, resp []byte, interrupted bool) error {
	r := &nodeReader{b: resp}
	return p.decodeEnvelope(ctx, r, interrupted)
}

// decodeExistsNodeResult decodes an ok envelope carrying u8 exists + node.
func (p *Python) decodeExistsNodeResult(ctx context.Context, resp []byte, interrupted bool) (Value, bool, error) {
	r := &nodeReader{b: resp}
	if err := p.decodeEnvelope(ctx, r, interrupted); err != nil {
		return nil, false, err
	}
	exists := r.u8() != 0
	v, err := p.decodeNode(r)
	if err != nil {
		return nil, false, err
	}
	return v, exists, nil
}

// decodeEvalResult decodes py_eval's envelope: the result node (on ok), the
// raised exception, or the exit code, followed either way by the captured
// stdout/stderr.
func (p *Python) decodeEvalResult(ctx context.Context, resp []byte, interrupted bool) (Result, error) {
	r := &nodeReader{b: resp}
	res := Result{Value: None()}
	switch r.u8() {
	case wireOK:
		v, err := p.decodeNode(r)
		if err != nil {
			return Result{}, err
		}
		res.Value = v
	case wireRaised:
		pe, err := p.decodeRaised(r)
		if err != nil {
			return Result{}, err
		}
		if interrupted {
			return Result{}, ctx.Err()
		}
		res.Error = pe
	case wireExit:
		res.Error = &internal.ExitError{Code: int(int32(r.u32()))}
	default:
		return Result{}, fmt.Errorf("python: malformed eval envelope")
	}
	res.Stdout = r.lenString()
	res.Stderr = r.lenString()
	if r.fail {
		return Result{}, fmt.Errorf("python: malformed eval envelope")
	}
	return res, nil
}
