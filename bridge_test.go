package python

// The Go <-> Python bridge suite: typed values crossing in both directions,
// identity-preserving object handles, the container and object protocols,
// calling Python from Go (positional and keyword arguments), exposing Go
// functions to Python (NewFunction / Bind), error propagation both ways,
// re-entrancy, handle lifetime, and context cancellation of a call.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// mainModule returns the __main__ module handle.
func mainModule(t *testing.T, p *Python) ModuleValue {
	t.Helper()
	m, err := p.Import(ctx, "__main__")
	if err != nil {
		t.Fatalf("Import(__main__): %v", err)
	}
	return m
}

// evalAs evaluates the expression src and extracts T from its value.
func evalAs[T Value](t *testing.T, p *Python, src string) T {
	t.Helper()
	r := mustEval(t, p, src)
	v, err := As[T](r.Value)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return v
}

func int64Of(t *testing.T, v Value) int64 {
	t.Helper()
	n, err := As[IntValue](v)
	if err != nil {
		t.Fatalf("not an int: %v", err)
	}
	i, ok := n.Int64()
	if !ok {
		t.Fatalf("int does not fit in int64: %v", n.BigInt())
	}
	return i
}

func strOf(t *testing.T, v Value) string {
	t.Helper()
	s, err := As[StrValue](v)
	if err != nil {
		t.Fatalf("not a str: %v", err)
	}
	return s.String()
}

// TestCallWithArgsAndKwargs calls a Python function from Go with positional
// and keyword arguments and reads a tuple back.
func TestCallWithArgsAndKwargs(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "def f(a, b=2, *, c=3):\n    return (a, b, c)")
	f := evalAs[FunctionValue](t, p, "f")
	if f.TypeName() != "function" || !f.Callable() {
		t.Errorf("f: TypeName=%q Callable=%v", f.TypeName(), f.Callable())
	}

	res, err := f.Call(ctx, ValueOf(1), Kwarg("c", ValueOf(30)))
	if err != nil {
		t.Fatal(err)
	}
	tup, err := As[TupleValue](res)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := tup.Values(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 3 || int64Of(t, vals[0]) != 1 || int64Of(t, vals[1]) != 2 || int64Of(t, vals[2]) != 30 {
		t.Errorf("f(1, c=30) = %v", vals)
	}
	if n, err := tup.Len(ctx); err != nil || n != 3 {
		t.Errorf("Len = %d (%v)", n, err)
	}

	// CallMethod on the module: __main__.f(10, 20).
	res, err = mainModule(t, p).CallMethod(ctx, "f", ValueOf(10), ValueOf(20))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRepr(t, res); got != "(10, 20, 3)" {
		t.Errorf("__main__.f(10, 20) = %s", got)
	}

	// A wrong call raises TypeError as a *PythonError.
	_, err = f.Call(ctx)
	var pe *PythonError
	if !errors.As(err, &pe) || pe.Type != "TypeError" {
		t.Errorf("f() error = %v, want TypeError", err)
	}
}

// mustRepr is repr(v) for a handle-backed value.
func mustRepr(t *testing.T, v Value) string {
	t.Helper()
	o, err := As[ObjectValue](v)
	if err != nil {
		t.Fatalf("not an object: %v", err)
	}
	s, err := o.Repr(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestValueKindsAcrossBridge checks every by-value kind in both directions:
// Python literals decode to the right Go type and value, and Go values passed
// into Python arrive as the right Python type with the same value.
func TestValueKindsAcrossBridge(t *testing.T) {
	p := newPy(t, Config{})

	// Python -> Go.
	if v := evalAs[NoneValue](t, p, "None"); v.Kind() != KindNone {
		t.Error("None kind")
	}
	if v := evalAs[BoolValue](t, p, "True"); !v.Bool() {
		t.Error("True")
	}
	if v := evalAs[IntValue](t, p, "-42"); mustInt64(t, v) != -42 {
		t.Error("-42")
	}
	if v := evalAs[FloatValue](t, p, "1.5"); v.Float() != 1.5 {
		t.Error("1.5")
	}
	if v := evalAs[FloatValue](t, p, "float('nan')"); !math.IsNaN(v.Float()) {
		t.Error("nan")
	}
	if v := evalAs[ComplexValue](t, p, "1+2j"); v.Complex() != complex(1, 2) {
		t.Error("1+2j")
	}
	if v := evalAs[StrValue](t, p, "'héllo'"); v.String() != "héllo" {
		t.Error("str")
	}
	if v := evalAs[BytesValue](t, p, "b'\\x00\\xff'"); string(v.Bytes()) != "\x00\xff" {
		t.Error("bytes")
	}

	// Go -> Python: type name and equality as Python sees them.
	mustEval(t, p, "def tn(x):\n    return type(x).__name__\ndef same(x, y):\n    return x == y and type(x) is type(y)")
	m := mainModule(t, p)
	tn := func(v Value) string {
		t.Helper()
		r, err := m.CallMethod(ctx, "tn", v)
		if err != nil {
			t.Fatal(err)
		}
		return strOf(t, r)
	}
	same := func(v Value, expr string) bool {
		t.Helper()
		r, err := m.CallMethod(ctx, "same", v, evalAs[Value](t, p, expr))
		if err != nil {
			t.Fatal(err)
		}
		b, err := As[BoolValue](r)
		if err != nil {
			t.Fatal(err)
		}
		return b.Bool()
	}
	cases := []struct {
		v    Value
		name string
		expr string
	}{
		{None(), "NoneType", "None"},
		{ValueOf(true), "bool", "True"},
		{ValueOf(7), "int", "7"},
		{ValueOf(int8(-3)), "int", "-3"},
		{ValueOf(uint64(math.MaxUint64)), "int", "2**64 - 1"},
		{ValueOf(2.25), "float", "2.25"},
		{ValueOf(float32(0.5)), "float", "0.5"},
		{ValueOf(complex(3, -4)), "complex", "3-4j"},
		{ValueOf("wasm"), "str", "'wasm'"},
		{ValueOf([]byte("raw\x00")), "bytes", "b'raw\\x00'"},
		{ValueOf(new(big.Int).Lsh(big.NewInt(1), 100)), "int", "2**100"},
	}
	for _, c := range cases {
		if got := tn(c.v); got != c.name {
			t.Errorf("type of %v in Python = %s, want %s", c.v, got, c.name)
		}
		if !same(c.v, c.expr) {
			t.Errorf("%v != %s in Python", c.v, c.expr)
		}
	}
	// nil crosses as None.
	if got := tn(nil); got != "NoneType" {
		t.Errorf("nil -> %s", got)
	}
}

func mustInt64(t *testing.T, v IntValue) int64 {
	t.Helper()
	i, ok := v.Int64()
	if !ok {
		t.Fatalf("does not fit in int64: %v", v.BigInt())
	}
	return i
}

// TestBigIntRoundTrip pins arbitrary-precision ints: values outside the
// int64 range cross as bigints, in both directions, with sign.
func TestBigIntRoundTrip(t *testing.T) {
	p := newPy(t, Config{})
	for _, expr := range []string{"2**100", "-(2**100)", "2**63", "-(2**63) - 1", "10**40 + 7"} {
		v := evalAs[IntValue](t, p, expr)
		if !v.IsBig() {
			t.Errorf("%s: not big", expr)
		}
		if _, ok := v.Int64(); ok {
			t.Errorf("%s: Int64 reported ok", expr)
		}
		want := evalAs[StrValue](t, p, "str("+expr+")").String()
		if got := v.BigInt().String(); got != want {
			t.Errorf("%s = %s, want %s", expr, got, want)
		}
		// Back into Python: equal to the original.
		eq := evalAs[FunctionValue](t, p, "lambda x: x == "+expr)
		r, err := eq.Call(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := As[BoolValue](r); !b.Bool() {
			t.Errorf("%s did not round-trip", expr)
		}
	}
	// Boundary values stay small ints.
	for _, expr := range []string{"2**63 - 1", "-(2**63)"} {
		if v := evalAs[IntValue](t, p, expr); v.IsBig() {
			t.Errorf("%s: unexpectedly big", expr)
		}
	}
}

// TestStrSurrogateRoundTrip pins that a str holding a lone surrogate — not
// representable in UTF-8 — still crosses byte-exactly in both directions.
func TestStrSurrogateRoundTrip(t *testing.T) {
	p := newPy(t, Config{})
	v := evalAs[StrValue](t, p, "'a\\ud800b'")
	if len(v.String()) != 5 { // 'a' + 3-byte surrogatepass sequence + 'b'
		t.Errorf("surrogate str has %d bytes: %q", len(v.String()), v.String())
	}
	mustEval(t, p, "def check(s):\n    return s == 'a\\ud800b' and len(s) == 3")
	r, err := mainModule(t, p).CallMethod(ctx, "check", v)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := As[BoolValue](r); !b.Bool() {
		t.Error("surrogate str did not round-trip")
	}
}

// TestObjectIdentityRoundTrip pins the handle model: the same object always
// surfaces with an Equal handle, mutations through Go are seen by Python and
// vice versa, and the handle reports the Python-visible type name.
func TestObjectIdentityRoundTrip(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "class Counter:\n    def __init__(self):\n        self.n = 0\n    def inc(self):\n        self.n += 1\n        return self.n\nc = Counter()")

	c1 := evalAs[ObjectValue](t, p, "c")
	c2 := evalAs[ObjectValue](t, p, "c")
	if !c1.Equal(c2) {
		t.Error("same object, different handles")
	}
	if c1.TypeName() != "__main__.Counter" || c1.Callable() {
		t.Errorf("TypeName=%q Callable=%v", c1.TypeName(), c1.Callable())
	}
	if _, err := c1.CallMethod(ctx, "inc"); err != nil {
		t.Fatal(err)
	}
	if got := evalAs[IntValue](t, p, "c.n"); mustInt64(t, got) != 1 {
		t.Error("mutation through Go not visible to Python")
	}
	if err := c1.SetAttr(ctx, "n", ValueOf(41)); err != nil {
		t.Fatal(err)
	}
	if r := mustEval(t, p, "c.inc()"); int64Of(t, r.Value) != 42 {
		t.Error("SetAttr not visible to Python")
	}
	n, err := c2.Attr(ctx, "n")
	if err != nil || int64Of(t, n) != 42 {
		t.Errorf("Attr(n) = %v (%v)", n, err)
	}

	// The class is a ClassValue; calling it instantiates.
	cls := evalAs[ClassValue](t, p, "Counter")
	if cls.TypeName() != "type" || !cls.Callable() {
		t.Errorf("class: TypeName=%q Callable=%v", cls.TypeName(), cls.Callable())
	}
	inst, err := cls.Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := inst.(ObjectValue).IsInstance(ctx, cls); err != nil || !ok {
		t.Errorf("IsInstance = %v (%v)", ok, err)
	}
	if inst.(ObjectValue).Equal(c1) {
		t.Error("distinct instances compare Equal")
	}
	// Passing a handle back hands Python the same object.
	mustEval(t, p, "def is_c(x):\n    return x is c")
	r, err := mainModule(t, p).CallMethod(ctx, "is_c", c1)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := As[BoolValue](r); !b.Bool() {
		t.Error("handle did not dereference to the same object")
	}
	// A handle from another instance is refused.
	other := newPy(t, Config{})
	foreign := evalAs[ObjectValue](t, other, "object()")
	if _, err := mainModule(t, p).CallMethod(ctx, "is_c", foreign); err == nil || !strings.Contains(err.Error(), "different Python instance") {
		t.Errorf("foreign handle accepted: %v", err)
	}
}

// TestContainerOperations exercises the list, tuple, dict, set, and
// frozenset typed handles and the aggregate constructors.
func TestContainerOperations(t *testing.T) {
	p := newPy(t, Config{})

	lst, err := p.NewList(ctx, ValueOf(1), ValueOf("two"))
	if err != nil {
		t.Fatal(err)
	}
	if lst.Kind() != KindList || lst.TypeName() != "list" {
		t.Errorf("list kind/type: %s %s", lst.Kind(), lst.TypeName())
	}
	if err := lst.Append(ctx, ValueOf(3.0), None()); err != nil {
		t.Fatal(err)
	}
	if n, _ := lst.Len(ctx); n != 4 {
		t.Errorf("len = %d", n)
	}
	if err := lst.SetIndex(ctx, 0, ValueOf(100)); err != nil {
		t.Fatal(err)
	}
	if v, err := lst.Index(ctx, 0); err != nil || int64Of(t, v) != 100 {
		t.Errorf("Index(0) = %v (%v)", v, err)
	}
	if v, err := lst.Index(ctx, -1); err != nil || v.Kind() != KindNone {
		t.Errorf("Index(-1) = %v (%v)", v, err)
	}
	if _, err := lst.Index(ctx, 99); err == nil {
		t.Error("out-of-range Index succeeded")
	}
	vals, err := lst.Values(ctx)
	if err != nil || len(vals) != 4 || strOf(t, vals[1]) != "two" {
		t.Errorf("Values = %v (%v)", vals, err)
	}
	if ok, _ := lst.Contains(ctx, ValueOf("two")); !ok {
		t.Error("Contains(two) = false")
	}
	if err := lst.DelItem(ctx, ValueOf(1)); err != nil {
		t.Fatal(err)
	}
	if n, _ := lst.Len(ctx); n != 3 {
		t.Errorf("len after del = %d", n)
	}

	tup, err := p.NewTuple(ctx, ValueOf("a"), lst)
	if err != nil {
		t.Fatal(err)
	}
	if tup.Kind() != KindTuple {
		t.Error("tuple kind")
	}
	if v, _ := tup.Index(ctx, 1); !v.(ListValue).Equal(lst) {
		t.Error("tuple element is not the same list")
	}

	d, err := p.NewDict(ctx, DictItem{ValueOf("k"), ValueOf(1)}, DictItem{ValueOf(2), lst})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind() != KindDict {
		t.Error("dict kind")
	}
	if v, ok, err := d.Get(ctx, ValueOf("k")); err != nil || !ok || int64Of(t, v) != 1 {
		t.Errorf("Get(k) = %v %v %v", v, ok, err)
	}
	if v, ok, err := d.Get(ctx, ValueOf("missing")); err != nil || ok || v.Kind() != KindNone {
		t.Errorf("Get(missing) = %v %v %v", v, ok, err)
	}
	if err := d.Set(ctx, ValueOf("k"), ValueOf("v")); err != nil {
		t.Fatal(err)
	}
	keys, err := d.Keys(ctx)
	if err != nil || len(keys) != 2 || strOf(t, keys[0]) != "k" {
		t.Errorf("Keys = %v (%v)", keys, err)
	}
	dvals, err := d.Values(ctx)
	if err != nil || len(dvals) != 2 || strOf(t, dvals[0]) != "v" {
		t.Errorf("Values = %v (%v)", dvals, err)
	}
	items, err := d.Items(ctx)
	if err != nil || len(items) != 2 || int64Of(t, items[1].Key) != 2 || !items[1].Value.(ListValue).Equal(lst) {
		t.Errorf("Items = %v (%v)", items, err)
	}
	if err := d.Delete(ctx, ValueOf(2)); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.Len(ctx); n != 1 {
		t.Errorf("dict len = %d", n)
	}
	if err := d.Delete(ctx, ValueOf("nope")); err == nil {
		t.Error("Delete of a missing key succeeded")
	}
	// The dict is the same object Python sees.
	if err := mainModule(t, p).SetAttr(ctx, "d", d); err != nil {
		t.Fatal(err)
	}
	if got := evalRepr(t, p, "d"); got != "{'k': 'v'}" {
		t.Errorf("d in Python = %s", got)
	}

	s, err := p.NewSet(ctx, ValueOf(1), ValueOf(1), ValueOf(2))
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind() != KindSet {
		t.Error("set kind")
	}
	if n, _ := s.Len(ctx); n != 2 {
		t.Errorf("set len = %d", n)
	}
	if err := s.Add(ctx, ValueOf(3)); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(ctx, ValueOf(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(ctx, ValueOf(99)); err != nil {
		t.Errorf("Discard of an absent element: %v", err)
	}
	if ok, _ := s.Contains(ctx, ValueOf(3)); !ok {
		t.Error("Contains(3) = false")
	}
	svals, _ := s.Values(ctx)
	if len(svals) != 2 {
		t.Errorf("set Values = %v", svals)
	}
	fs, err := p.NewFrozenSet(ctx, ValueOf("x"))
	if err != nil {
		t.Fatal(err)
	}
	if fs.Kind() != KindFrozenSet || fs.TypeName() != "frozenset" {
		t.Errorf("frozenset: %s %s", fs.Kind(), fs.TypeName())
	}
	if ok, _ := fs.Contains(ctx, ValueOf("x")); !ok {
		t.Error("frozenset Contains")
	}
	// Item on a dict and a str-keyed lookup through the generic protocol.
	if v, err := d.Item(ctx, ValueOf("k")); err != nil || strOf(t, v) != "v" {
		t.Errorf("Item(k) = %v (%v)", v, err)
	}
}

// TestSubclassesCrossAsHandles pins the classification rule: subclass
// instances of the immutable built-ins keep their identity (they cross as
// objects, not values), while container subclasses get the container's
// typed handle and report their real type name.
func TestSubclassesCrossAsHandles(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "import enum, collections\nclass Color(enum.IntEnum):\n    RED = 1\nclass S(str):\n    pass\nPoint = collections.namedtuple('Point', 'x y')")

	red := evalAs[Value](t, p, "Color.RED")
	o, ok := red.(ObjectValue)
	if !ok || o.TypeName() != "__main__.Color" {
		t.Fatalf("IntEnum member crossed as %T %v", red, red)
	}
	if v, err := o.Attr(ctx, "value"); err != nil || int64Of(t, v) != 1 {
		t.Errorf("Color.RED.value = %v (%v)", v, err)
	}
	if _, isInt := red.(IntValue); isInt {
		t.Error("IntEnum member crossed by value")
	}
	if _, isStr := evalAs[Value](t, p, "S('x')").(StrValue); isStr {
		t.Error("str subclass crossed by value")
	}
	od := evalAs[DictValue](t, p, "collections.OrderedDict(a=1)")
	if od.TypeName() != "collections.OrderedDict" {
		t.Errorf("OrderedDict type name = %q", od.TypeName())
	}
	pt := evalAs[TupleValue](t, p, "Point(1, 2)")
	if pt.TypeName() != "__main__.Point" {
		t.Errorf("namedtuple type name = %q", pt.TypeName())
	}
	if v, err := pt.Attr(ctx, "y"); err != nil || int64Of(t, v) != 2 {
		t.Errorf("Point.y = %v (%v)", v, err)
	}
	// Other handle kinds.
	if m := evalAs[ModuleValue](t, p, "enum"); m.TypeName() != "module" {
		t.Errorf("module type name = %q", m.TypeName())
	}
	if f := evalAs[FunctionValue](t, p, "len"); f.TypeName() != "builtin_function_or_method" {
		t.Errorf("builtin type name = %q", f.TypeName())
	}
	if f := evalAs[FunctionValue](t, p, "[].append"); f.Kind() != KindFunction {
		t.Errorf("bound method kind = %s", f.Kind())
	}
	if g := evalAs[Value](t, p, "(x for x in range(3))"); g.Kind() != KindObject {
		t.Errorf("generator kind = %s", g.Kind())
	}
	if b := evalAs[Value](t, p, "bytearray(b'x')"); b.Kind() != KindObject {
		t.Errorf("bytearray kind = %s", b.Kind())
	}
}

// TestImportAndModuleProtocol imports a stdlib module and drives it through
// the object protocol.
func TestImportAndModuleProtocol(t *testing.T) {
	p := newPy(t, Config{})
	js, err := p.Import(ctx, "json")
	if err != nil {
		t.Fatal(err)
	}
	if js.Kind() != KindModule || js.TypeName() != "module" {
		t.Errorf("json: %s %s", js.Kind(), js.TypeName())
	}
	d, err := p.NewDict(ctx, DictItem{ValueOf("a"), ValueOf(1)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := js.CallMethod(ctx, "dumps", d, Kwarg("sort_keys", ValueOf(true)))
	if err != nil {
		t.Fatal(err)
	}
	if strOf(t, out) != `{"a": 1}` {
		t.Errorf("json.dumps = %v", out)
	}
	s, err := js.Str(ctx)
	if err != nil || !strings.HasPrefix(s, "<module 'json'") {
		t.Errorf("Str = %q (%v)", s, err)
	}
	if _, err := p.Import(ctx, "no_such_module_xyz"); err == nil {
		t.Error("importing a missing module succeeded")
	} else if pe := new(PythonError); !errors.As(err, &pe) || pe.Type != "ModuleNotFoundError" {
		t.Errorf("import error = %v", err)
	}
	// A dotted import returns the leaf module.
	leaf, err := p.Import(ctx, "os.path")
	if err != nil {
		t.Fatal(err)
	}
	if name, err := leaf.Attr(ctx, "__name__"); err != nil || !strings.HasSuffix(strOf(t, name), "path") {
		t.Errorf("os.path.__name__ = %v (%v)", name, err)
	}
}

// TestCallRaisesPythonError pins error propagation Python -> Go for calls:
// the exception is a *PythonError with the live instance.
func TestCallRaisesPythonError(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "def boom():\n    raise KeyError('k')")
	_, err := mainModule(t, p).CallMethod(ctx, "boom")
	var pe *PythonError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v", err, err)
	}
	if pe.Type != "KeyError" || pe.Message != "'k'" || pe.Value == nil {
		t.Errorf("PythonError = %+v", pe)
	}
	if !strings.Contains(pe.Traceback, "in boom") {
		t.Errorf("traceback lacks the frame: %q", pe.Traceback)
	}
	// Attribute and item failures too.
	if _, err := pe.Value.Attr(ctx, "nope"); err == nil {
		t.Error("missing attribute succeeded")
	} else if !errors.As(err, &pe) || pe.Type != "AttributeError" {
		t.Errorf("Attr error = %v", err)
	}
}

// TestNewFunctionAndBind exposes Go functions to Python: Bind installs a
// global; NewFunction returns a first-class function value usable as an
// argument; positional and keyword arguments arrive typed; the return value
// crosses back.
func TestNewFunctionAndBind(t *testing.T) {
	p := newPy(t, Config{})

	err := p.Bind("go_upper", func(args []Value, kwargs map[string]Value) (Value, error) {
		s, err := As[StrValue](args[0])
		if err != nil {
			return nil, err
		}
		return ValueOf(strings.ToUpper(s.String())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := evalAs[StrValue](t, p, `go_upper("hello")`); got.String() != "HELLO" {
		t.Errorf("go_upper = %q", got.String())
	}

	// kwargs and the None default.
	err = p.Bind("go_join", func(args []Value, kwargs map[string]Value) (Value, error) {
		sep := ","
		if v, ok := kwargs["sep"]; ok {
			sep = strOf(t, v)
		}
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = fmt.Sprint(int64Of(t, a))
		}
		return ValueOf(strings.Join(parts, sep)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := evalAs[StrValue](t, p, "go_join(1, 2, 3, sep='-')"); got.String() != "1-2-3" {
		t.Errorf("go_join = %q", got.String())
	}
	if got := evalAs[StrValue](t, p, "go_join(4, 5)"); got.String() != "4,5" {
		t.Errorf("go_join default = %q", got.String())
	}
	err = p.Bind("go_none", func([]Value, map[string]Value) (Value, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if v := evalAs[Value](t, p, "go_none()"); v.Kind() != KindNone {
		t.Errorf("nil result = %v", v)
	}

	// NewFunction: a first-class value with the given __name__, usable as an
	// argument (a sort key) without being bound to a name.
	twice, err := p.NewFunction("twice", func(args []Value, _ map[string]Value) (Value, error) {
		return ValueOf(2 * int64Of(t, args[0])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if name, err := twice.Attr(ctx, "__name__"); err != nil || strOf(t, name) != "twice" {
		t.Errorf("__name__ = %v (%v)", name, err)
	}
	mustEval(t, p, "def apply(f, xs):\n    return [f(x) for x in xs]")
	xs, _ := p.NewList(ctx, ValueOf(1), ValueOf(2))
	r, err := mainModule(t, p).CallMethod(ctx, "apply", twice, xs)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRepr(t, r); got != "[2, 4]" {
		t.Errorf("apply(twice) = %s", got)
	}
	if _, err := p.NewFunction("nil", nil); err == nil {
		t.Error("nil GoFunc accepted")
	}
}

// TestBindReceivesObjects: a Go function receives handles for object
// arguments and can mutate them in place; the caller sees the change.
func TestBindReceivesObjects(t *testing.T) {
	p := newPy(t, Config{})
	var kept ObjectValue
	err := p.Bind("go_touch", func(args []Value, _ map[string]Value) (Value, error) {
		d, err := As[DictValue](args[0])
		if err != nil {
			return nil, err
		}
		if err := d.Set(ctx, ValueOf("touched"), ValueOf(true)); err != nil {
			return nil, err
		}
		kept = d
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mustEval(t, p, "d = {}\nres = go_touch(d)")
	if got := evalRepr(t, p, "d"); got != "{'touched': True}" {
		t.Errorf("d = %s", got)
	}
	if got := evalRepr(t, p, "res is d"); got != "True" {
		t.Errorf("returned handle is not the same object: %s", got)
	}
	// The handler may keep the handle beyond the call.
	if n, err := kept.Len(ctx); err != nil || n != 1 {
		t.Errorf("kept handle: %d %v", n, err)
	}
}

// TestGoErrorsRaiseInPython pins error propagation Go -> Python: a Go error
// raises RuntimeError with its text, a contained panic likewise, and a
// *PythonError returned by the Go function re-raises the very same exception
// instance (so `is` holds and the type is preserved).
func TestGoErrorsRaiseInPython(t *testing.T) {
	p := newPy(t, Config{})

	if err := p.Bind("go_fail", func([]Value, map[string]Value) (Value, error) {
		return nil, errors.New("nope from go")
	}); err != nil {
		t.Fatal(err)
	}
	if out := pyTry(t, p, "go_fail()"); out != "RuntimeError: nope from go" {
		t.Errorf("go error -> %q", out)
	}

	if err := p.Bind("go_panic", func([]Value, map[string]Value) (Value, error) {
		panic("kaboom")
	}); err != nil {
		t.Fatal(err)
	}
	if out := pyTry(t, p, "go_panic()"); !strings.HasPrefix(out, "RuntimeError: Go function panicked: kaboom") {
		t.Errorf("go panic -> %q", out)
	}

	// Re-raise: the Go function calls back into Python, which raises a
	// pre-built exception instance; Go returns the *PythonError unchanged.
	mustEval(t, p, "original = ValueError('orig')\ndef raiser():\n    raise original")
	m := mainModule(t, p)
	if err := p.Bind("go_reraise", func([]Value, map[string]Value) (Value, error) {
		_, err := m.CallMethod(ctx, "raiser")
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	mustEval(t, p, "try:\n    go_reraise()\nexcept ValueError as e:\n    caught = e")
	if got := evalRepr(t, p, "caught is original"); got != "True" {
		t.Errorf("re-raised exception is not the same instance: %s", got)
	}
	// A wrapped *PythonError still re-raises the instance.
	if err := p.Bind("go_wrap", func([]Value, map[string]Value) (Value, error) {
		_, err := m.CallMethod(ctx, "raiser")
		return nil, fmt.Errorf("wrapped: %w", err)
	}); err != nil {
		t.Fatal(err)
	}
	mustEval(t, p, "try:\n    go_wrap()\nexcept ValueError as e:\n    caught2 = e")
	if got := evalRepr(t, p, "caught2 is original"); got != "True" {
		t.Errorf("wrapped re-raise is not the same instance: %s", got)
	}
}

// TestReentrantCallback: a Go function invoked from Python calls back into
// the same instance (Eval and a method call) before returning.
func TestReentrantCallback(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "def square(x):\n    return x * x")
	m := mainModule(t, p)
	err := p.Bind("go_sum_squares", func(args []Value, _ map[string]Value) (Value, error) {
		var total int64
		for _, a := range args {
			r, err := m.CallMethod(ctx, "square", a)
			if err != nil {
				return nil, err
			}
			total += int64Of(t, r)
		}
		res, err := p.Eval(ctx, fmt.Sprintf("%d + 0", total))
		if err != nil {
			return nil, err
		}
		if res.Error != nil {
			return nil, res.Error
		}
		return res.Value, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := evalAs[IntValue](t, p, "go_sum_squares(1, 2, 3)"); mustInt64(t, got) != 14 {
		t.Errorf("go_sum_squares = %v", got)
	}
	// Three levels: Python -> Go -> Python -> Go.
	if err := p.Bind("go_depth", func(args []Value, _ map[string]Value) (Value, error) {
		n := int64Of(t, args[0])
		if n == 0 {
			return ValueOf(0), nil
		}
		r := mustEval(t, p, fmt.Sprintf("go_depth(%d) + 1", n-1))
		return r.Value, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := evalAs[IntValue](t, p, "go_depth(5)"); mustInt64(t, got) != 5 {
		t.Errorf("go_depth(5) = %v", got)
	}
}

// TestCallbackDoesNotDeadlockOtherGoroutines pins that a Go function running
// on behalf of the guest does not hold the instance lock: a call into the
// same instance from another goroutine completes while the handler waits.
func TestCallbackDoesNotDeadlockOtherGoroutines(t *testing.T) {
	p := newPy(t, Config{})
	var wg sync.WaitGroup
	err := p.Bind("go_wait", func([]Value, map[string]Value) (Value, error) {
		wg.Add(1)
		done := make(chan error, 1)
		go func() {
			defer wg.Done()
			r, err := p.Eval(ctx, "40 + 2")
			if err == nil && r.Error != nil {
				err = r.Error
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				return nil, err
			}
			return ValueOf("ok"), nil
		case <-time.After(20 * time.Second):
			return nil, errors.New("other goroutine's Eval did not complete: deadlock")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := evalAs[StrValue](t, p, "go_wait()"); got.String() != "ok" {
		t.Errorf("go_wait = %q", got.String())
	}
	wg.Wait()
}

// TestHandleKeepsObjectAlive pins the pin/release lifecycle: an object only
// referenced from Go survives Python dropping it, and is released after the
// Go handle becomes unreachable, the GC runs, and the next call drains the
// release queue.
func TestHandleKeepsObjectAlive(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "import weakref\nclass Thing:\n    pass\nt = Thing()\nwr = weakref.ref(t)")

	h := evalAs[ObjectValue](t, p, "t")
	mustEval(t, p, "del t")
	if got := evalRepr(t, p, "wr() is None"); got != "False" {
		t.Fatal("object collected while a Go handle pinned it")
	}
	if h.TypeName() != "__main__.Thing" {
		t.Errorf("type name = %q", h.TypeName())
	}
	// Drop the handle. The finalizer only queues the release; the next
	// user-initiated call drains it.
	h = nil
	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		runtime.Gosched()
		if got := evalRepr(t, p, "wr() is None"); got == "True" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("object not released after the Go handle was dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCallContextCancel pins that a runaway Python call started from Go is
// interrupted by cancelling the context.
func TestCallContextCancel(t *testing.T) {
	p := newPy(t, Config{})
	mustEval(t, p, "def spin():\n    while True:\n        pass")
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_, err := mainModule(t, p).CallMethod(cctx, "spin")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallMethod(spin) err = %v, want context.DeadlineExceeded", err)
	}
	if got := evalRepr(t, p, "1+1"); got != "2" {
		t.Errorf("instance unusable after cancellation: %q", got)
	}
}

// TestValueHelpers covers the pure-Go parts of the value model.
func TestValueHelpers(t *testing.T) {
	if k := KindFrozenSet; k.String() != "frozenset" {
		t.Errorf("Kind.String = %q", k.String())
	}
	if k := Kind(99); k.String() != "kind(99)" {
		t.Errorf("Kind(99).String = %q", k.String())
	}
	if _, err := As[IntValue](ValueOf("x")); err == nil || !strings.Contains(err.Error(), "value is str") {
		t.Errorf("As mismatch error = %v", err)
	}
	if _, err := As[IntValue](nil); err == nil {
		t.Error("As(nil) succeeded")
	}
	if v := ValueOf((*big.Int)(nil)); v.Kind() != KindNone {
		t.Error("nil *big.Int is not None")
	}
	b := []byte("abc")
	bv := ValueOf(b).(BytesValue)
	b[0] = 'z'
	if string(bv.Bytes()) != "abc" {
		t.Error("BytesValue aliases the caller's slice")
	}
	big1 := ValueOf(new(big.Int).SetUint64(math.MaxUint64)).(IntValue)
	if !big1.IsBig() || big1.BigInt().String() != "18446744073709551615" {
		t.Errorf("uint64 max as big int = %v", big1.BigInt())
	}
	small := ValueOf(big.NewInt(5)).(IntValue)
	if small.IsBig() {
		t.Error("small *big.Int reported big")
	}
	if i, ok := small.Int64(); !ok || i != 5 {
		t.Errorf("Int64 = %d %v", i, ok)
	}
}
