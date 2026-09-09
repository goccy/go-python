package python

// The typed Value model: every Python value that crosses the bridge is one
// of the sealed concrete types below, following Python's own data model.
// Exact instances of the immutable built-ins (None, bool, int, float,
// complex, str, bytes) cross by value; every other object crosses by an
// identity-preserving handle and is one of the ObjectValue implementations
// in object.go.

import (
	"fmt"
	"math/big"
	"strconv"
)

// Kind is the runtime kind of a Value (mirroring reflect.Value.Kind): one
// kind per concrete type.
type Kind uint8

const (
	// KindNone is None.
	KindNone Kind = iota
	// KindBool is a bool.
	KindBool
	// KindInt is an int (of any magnitude).
	KindInt
	// KindFloat is a float.
	KindFloat
	// KindComplex is a complex.
	KindComplex
	// KindStr is a str.
	KindStr
	// KindBytes is a bytes.
	KindBytes
	// KindList is a list (or a list subclass).
	KindList
	// KindTuple is a tuple (or a tuple subclass, e.g. a namedtuple).
	KindTuple
	// KindDict is a dict (or a dict subclass, e.g. an OrderedDict).
	KindDict
	// KindSet is a set.
	KindSet
	// KindFrozenSet is a frozenset.
	KindFrozenSet
	// KindFunction is a routine: a function, a builtin function, a bound
	// method, or a method descriptor (what inspect.isroutine accepts).
	KindFunction
	// KindClass is a class (an instance of type).
	KindClass
	// KindModule is a module.
	KindModule
	// KindObject is any other object: an instance, an exception, a
	// generator, a bytearray, ...
	KindObject
)

var kindNames = [...]string{
	"none", "bool", "int", "float", "complex", "str", "bytes",
	"list", "tuple", "dict", "set", "frozenset",
	"function", "class", "module", "object",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Value is any Python value. Sealed: the concrete types — NoneValue,
// BoolValue, IntValue, FloatValue, ComplexValue, StrValue, BytesValue,
// ListValue, TupleValue, DictValue, SetValue, FrozenSetValue,
// FunctionValue, ClassValue, ModuleValue, and the plain object behind the
// ObjectValue interface — are the only implementations, so a type switch
// over them (with an ObjectValue case last) is exhaustive.
type Value interface {
	Kind() Kind
	sealed()
}

// Arg is one argument of a Python call: a Value (a positional argument) or
// a Kwarg (a keyword argument). It is what Call and CallMethod accept, so
// f(1, sep=",") is written f.Call(ctx, ValueOf(1), Kwarg("sep", ValueOf(","))).
type Arg interface {
	sealed()
}

// kwarg is a keyword argument (see Kwarg).
type kwarg struct {
	name string
	v    Value
}

func (kwarg) sealed() {}

// Kwarg is the keyword argument name=v in a Call / CallMethod argument
// list.
func Kwarg(name string, v Value) Arg {
	if v == nil {
		v = None()
	}
	return kwarg{name: name, v: v}
}

// As extracts the concrete type T from v, for call sites that know the
// expected type up front; a kind mismatch is an error, never a panic. T may
// also be the ObjectValue interface, which accepts every handle-backed
// value. For runtime inspection use a type switch instead.
func As[T Value](v Value) (T, error) {
	if t, ok := v.(T); ok {
		return t, nil
	}
	var zero T
	if v == nil {
		return zero, fmt.Errorf("python: value is nil, not %T", zero)
	}
	return zero, fmt.Errorf("python: value is %s, not %T", v.Kind(), zero)
}

// ---- values that cross by value --------------------------------------------

// NoneValue is None.
type NoneValue struct{}

func (NoneValue) sealed() {}

// Kind reports KindNone.
func (NoneValue) Kind() Kind { return KindNone }

// None returns the None value.
func None() NoneValue { return NoneValue{} }

// BoolValue is a bool.
type BoolValue struct{ b bool }

func (BoolValue) sealed() {}

// Kind reports KindBool.
func (BoolValue) Kind() Kind { return KindBool }

// Bool returns the boolean.
func (v BoolValue) Bool() bool { return v.b }

// IntValue is an int. Python ints are arbitrary-precision: Int64 reports
// whether the value fits in an int64, BigInt always returns it in full.
type IntValue struct {
	i   int64
	big *big.Int // non-nil only when the value does not fit in an int64
}

func (IntValue) sealed() {}

// Kind reports KindInt.
func (IntValue) Kind() Kind { return KindInt }

// Int64 returns the value and true when it fits in an int64; 0 and false
// otherwise (use BigInt).
func (v IntValue) Int64() (int64, bool) {
	if v.big != nil {
		return 0, false
	}
	return v.i, true
}

// IsBig reports whether the value does not fit in an int64.
func (v IntValue) IsBig() bool { return v.big != nil }

// BigInt returns the value as a fresh *big.Int, whatever its magnitude.
func (v IntValue) BigInt() *big.Int {
	if v.big != nil {
		return new(big.Int).Set(v.big)
	}
	return big.NewInt(v.i)
}

// FloatValue is a float.
type FloatValue struct{ f float64 }

func (FloatValue) sealed() {}

// Kind reports KindFloat.
func (FloatValue) Kind() Kind { return KindFloat }

// Float returns the float.
func (v FloatValue) Float() float64 { return v.f }

// ComplexValue is a complex.
type ComplexValue struct{ c complex128 }

func (ComplexValue) sealed() {}

// Kind reports KindComplex.
func (ComplexValue) Kind() Kind { return KindComplex }

// Complex returns the complex number.
func (v ComplexValue) Complex() complex128 { return v.c }

// StrValue is a str. It holds the text as UTF-8; a str containing lone
// surrogates round-trips through the "surrogatepass" encoding, so String
// may hold the corresponding (non-UTF-8-valid) byte sequences.
type StrValue struct{ s string }

func (StrValue) sealed() {}

// Kind reports KindStr.
func (StrValue) Kind() Kind { return KindStr }

// String returns the text.
func (v StrValue) String() string { return v.s }

// BytesValue is a bytes.
type BytesValue struct{ b []byte }

func (BytesValue) sealed() {}

// Kind reports KindBytes.
func (BytesValue) Kind() Kind { return KindBytes }

// Bytes returns a copy of the bytes.
func (v BytesValue) Bytes() []byte {
	out := make([]byte, len(v.b))
	copy(out, v.b)
	return out
}

// BasicType enumerates the Go types ValueOf converts, each to the Python
// built-in of the same shape.
type BasicType interface {
	bool |
		int | int8 | int16 | int32 | int64 |
		uint | uint8 | uint16 | uint32 | uint64 |
		float32 | float64 |
		complex64 | complex128 |
		string | []byte | *big.Int
}

// ValueOf converts a Go value to the Python value of the same shape: bool
// -> bool, every integer type and *big.Int -> int, float32/64 -> float,
// complex64/128 -> complex, string -> str, []byte -> bytes (copied). A nil
// *big.Int is None.
func ValueOf[T BasicType](v T) Value {
	switch x := any(v).(type) {
	case bool:
		return BoolValue{b: x}
	case int:
		return IntValue{i: int64(x)}
	case int8:
		return IntValue{i: int64(x)}
	case int16:
		return IntValue{i: int64(x)}
	case int32:
		return IntValue{i: int64(x)}
	case int64:
		return IntValue{i: x}
	case uint:
		return intFromUint64(uint64(x))
	case uint8:
		return IntValue{i: int64(x)}
	case uint16:
		return IntValue{i: int64(x)}
	case uint32:
		return IntValue{i: int64(x)}
	case uint64:
		return intFromUint64(x)
	case float32:
		return FloatValue{f: float64(x)}
	case float64:
		return FloatValue{f: x}
	case complex64:
		return ComplexValue{c: complex128(x)}
	case complex128:
		return ComplexValue{c: x}
	case string:
		return StrValue{s: x}
	case []byte:
		b := make([]byte, len(x))
		copy(b, x)
		return BytesValue{b: b}
	case *big.Int:
		if x == nil {
			return None()
		}
		return intFromBig(x)
	}
	panic("unreachable: BasicType exhausted")
}

func intFromUint64(u uint64) IntValue {
	if u <= 1<<63-1 {
		return IntValue{i: int64(u)}
	}
	return IntValue{big: new(big.Int).SetUint64(u)}
}

func intFromBig(b *big.Int) IntValue {
	if b.IsInt64() {
		return IntValue{i: b.Int64()}
	}
	return IntValue{big: new(big.Int).Set(b)}
}
