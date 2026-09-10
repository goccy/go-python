package python

// Handle-backed values: everything that is not an exact immutable built-in
// crosses the bridge as an identity-preserving handle. ObjectValue is the
// Python object protocol every such value implements; ListValue, TupleValue,
// DictValue, SetValue, FrozenSetValue, FunctionValue, ClassValue, and
// ModuleValue add the operations specific to their built-in type, and the
// unexported object type is the plain object behind the interface for
// everything else (instances, exceptions, generators, ...).

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
)

// objectHandle is one pin on a guest object: the registry id and the facts
// the object node carried. Every concrete handle type wraps one; the guest
// registry entry is released when the wrapper becomes unreachable (the
// finalizer queues the release, the next guest call drains the queue), or
// when the instance closes.
type objectHandle struct {
	p        *Python
	id       uint64
	kind     uint8
	callable bool
	typeName string
	// released flips once the release has been queued so a handle never
	// double-releases.
	released atomic.Bool
}

func newObjectHandle(p *Python, id uint64, kind uint8, callable bool, typeName string) *objectHandle {
	h := &objectHandle{p: p, id: id, kind: kind, callable: callable, typeName: typeName}
	runtime.SetFinalizer(h, func(h *objectHandle) {
		if h.released.CompareAndSwap(false, true) {
			h.p.raw.QueueRelease(h.id)
		}
	})
	return h
}

// ObjectValue is the Python object protocol: what every value that crosses
// by handle supports, whatever its concrete type. The concrete types are
// ListValue, TupleValue, DictValue, SetValue, FrozenSetValue,
// FunctionValue, ClassValue, ModuleValue, and the plain object for
// everything else; a type switch lists the concrete cases first and an
// ObjectValue case last.
//
// Every method that takes a ctx runs real Python code in the interpreter
// (an attribute access runs descriptors and __getattr__, an item access
// runs __getitem__, ...), so the semantics are exactly Python's. A Python
// exception is returned as a *PythonError.
type ObjectValue interface {
	Value
	// TypeName is the object's type as Python prints it: "list",
	// "collections.OrderedDict", "__main__.Counter". Known without a call.
	TypeName() string
	// Callable reports callable(obj). Known without a call.
	Callable() bool
	// Equal reports whether other is the same object (Python's `is`).
	Equal(other ObjectValue) bool
	// Attr is getattr(obj, name).
	Attr(ctx context.Context, name string) (Value, error)
	// SetAttr is setattr(obj, name, v).
	SetAttr(ctx context.Context, name string, v Value) error
	// Item is obj[key].
	Item(ctx context.Context, key Value) (Value, error)
	// SetItem is obj[key] = v.
	SetItem(ctx context.Context, key, v Value) error
	// DelItem is del obj[key].
	DelItem(ctx context.Context, key Value) error
	// Len is len(obj).
	Len(ctx context.Context) (int, error)
	// Contains is v in obj.
	Contains(ctx context.Context, v Value) (bool, error)
	// Call is obj(*args, **kwargs): Values are positional arguments, Kwarg
	// entries keyword arguments.
	Call(ctx context.Context, args ...Arg) (Value, error)
	// CallMethod is obj.name(*args, **kwargs) in one crossing.
	CallMethod(ctx context.Context, name string, args ...Arg) (Value, error)
	// Str is str(obj).
	Str(ctx context.Context) (string, error)
	// Repr is repr(obj).
	Repr(ctx context.Context) (string, error)
	// IsInstance is isinstance(obj, cls).
	IsInstance(ctx context.Context, cls ObjectValue) (bool, error)

	// handle seals the interface to this package's types.
	handle() *objectHandle
}

// object is the plain object behind ObjectValue and the shared
// implementation every typed handle embeds.
type object struct{ h *objectHandle }

func (object) sealed() {}

// Kind reports KindObject.
func (object) Kind() Kind { return KindObject }

func (o object) handle() *objectHandle { return o.h }

func (o object) TypeName() string { return o.h.typeName }

func (o object) Callable() bool { return o.h.callable }

func (o object) Equal(other ObjectValue) bool {
	if other == nil {
		return false
	}
	oh := other.handle()
	return oh != nil && oh.p == o.h.p && oh.id == o.h.id
}

// raw returns the instance machinery, or the closed/released error.
func (o object) raw() (*Python, error) {
	if o.h.released.Load() {
		return nil, fmt.Errorf("python: object handle has been released")
	}
	return o.h.p, nil
}

func (o object) Attr(ctx context.Context, name string) (Value, error) {
	p, err := o.raw()
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.GetAttrOp(ctx, o.h.id, name)
	if err != nil {
		return nil, err
	}
	return p.decodeNodeResult(ctx, resp, interrupted)
}

func (o object) SetAttr(ctx context.Context, name string, v Value) error {
	p, err := o.raw()
	if err != nil {
		return err
	}
	val, err := p.encodeSingle(v)
	if err != nil {
		return err
	}
	resp, interrupted, err := p.raw.SetAttrOp(ctx, o.h.id, name, val)
	if err != nil {
		return err
	}
	return p.decodeEmptyResult(ctx, resp, interrupted)
}

func (o object) Item(ctx context.Context, key Value) (Value, error) {
	p, err := o.raw()
	if err != nil {
		return nil, err
	}
	k, err := p.encodeSingle(key)
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.GetItemOp(ctx, o.h.id, k)
	if err != nil {
		return nil, err
	}
	return p.decodeNodeResult(ctx, resp, interrupted)
}

func (o object) SetItem(ctx context.Context, key, v Value) error {
	p, err := o.raw()
	if err != nil {
		return err
	}
	k, err := p.encodeSingle(key)
	if err != nil {
		return err
	}
	val, err := p.encodeSingle(v)
	if err != nil {
		return err
	}
	resp, interrupted, err := p.raw.SetItemOp(ctx, o.h.id, k, val)
	if err != nil {
		return err
	}
	return p.decodeEmptyResult(ctx, resp, interrupted)
}

func (o object) DelItem(ctx context.Context, key Value) error {
	p, err := o.raw()
	if err != nil {
		return err
	}
	k, err := p.encodeSingle(key)
	if err != nil {
		return err
	}
	resp, interrupted, err := p.raw.DelItemOp(ctx, o.h.id, k)
	if err != nil {
		return err
	}
	return p.decodeEmptyResult(ctx, resp, interrupted)
}

func (o object) Len(ctx context.Context) (int, error) {
	p, err := o.raw()
	if err != nil {
		return 0, err
	}
	resp, interrupted, err := p.raw.LenOp(ctx, o.h.id)
	if err != nil {
		return 0, err
	}
	n, err := p.decodeI64Result(ctx, resp, interrupted)
	return int(n), err
}

func (o object) Contains(ctx context.Context, v Value) (bool, error) {
	p, err := o.raw()
	if err != nil {
		return false, err
	}
	val, err := p.encodeSingle(v)
	if err != nil {
		return false, err
	}
	resp, interrupted, err := p.raw.ContainsOp(ctx, o.h.id, val)
	if err != nil {
		return false, err
	}
	return p.decodeBoolResult(ctx, resp, interrupted)
}

func (o object) Call(ctx context.Context, args ...Arg) (Value, error) {
	p, err := o.raw()
	if err != nil {
		return nil, err
	}
	pos, kw, err := p.encodeCallArgs(args)
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.CallOp(ctx, o.h.id, pos, kw)
	if err != nil {
		return nil, err
	}
	return p.decodeNodeResult(ctx, resp, interrupted)
}

func (o object) CallMethod(ctx context.Context, name string, args ...Arg) (Value, error) {
	p, err := o.raw()
	if err != nil {
		return nil, err
	}
	pos, kw, err := p.encodeCallArgs(args)
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.CallMethodOp(ctx, o.h.id, name, pos, kw)
	if err != nil {
		return nil, err
	}
	return p.decodeNodeResult(ctx, resp, interrupted)
}

func (o object) Str(ctx context.Context) (string, error) {
	p, err := o.raw()
	if err != nil {
		return "", err
	}
	resp, interrupted, err := p.raw.StrOp(ctx, o.h.id)
	if err != nil {
		return "", err
	}
	return p.decodeTextResult(ctx, resp, interrupted)
}

func (o object) Repr(ctx context.Context) (string, error) {
	p, err := o.raw()
	if err != nil {
		return "", err
	}
	resp, interrupted, err := p.raw.ReprOp(ctx, o.h.id)
	if err != nil {
		return "", err
	}
	return p.decodeTextResult(ctx, resp, interrupted)
}

func (o object) IsInstance(ctx context.Context, cls ObjectValue) (bool, error) {
	p, err := o.raw()
	if err != nil {
		return false, err
	}
	if cls == nil || cls.handle() == nil {
		return false, fmt.Errorf("python: IsInstance: nil class")
	}
	ch := cls.handle()
	if ch.p != p {
		return false, fmt.Errorf("python: class belongs to a different Python instance")
	}
	if ch.released.Load() {
		return false, fmt.Errorf("python: class handle has been released")
	}
	resp, interrupted, err := p.raw.IsInstanceOp(ctx, o.h.id, ch.id)
	if err != nil {
		return false, err
	}
	return p.decodeBoolResult(ctx, resp, interrupted)
}

// values is list(obj): the elements iteration yields.
func (o object) values(ctx context.Context) ([]Value, error) {
	p, err := o.raw()
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.IterOp(ctx, o.h.id)
	if err != nil {
		return nil, err
	}
	return p.decodeListResult(ctx, resp, interrupted)
}

// index is obj[i] for an integer index.
func (o object) index(ctx context.Context, i int) (Value, error) {
	return o.Item(ctx, ValueOf(i))
}

// ---- ListValue (list) --------------------------------------------------------

// ListValue is a list (or a list subclass). Elements are read and written
// in place in the interpreter.
type ListValue struct{ object }

// Kind reports KindList.
func (ListValue) Kind() Kind { return KindList }

// Index is l[i].
func (l ListValue) Index(ctx context.Context, i int) (Value, error) { return l.index(ctx, i) }

// SetIndex is l[i] = v.
func (l ListValue) SetIndex(ctx context.Context, i int, v Value) error {
	return l.SetItem(ctx, ValueOf(i), v)
}

// Append is l.append(v) for each v, in order.
func (l ListValue) Append(ctx context.Context, vals ...Value) error {
	p, err := l.raw()
	if err != nil {
		return err
	}
	enc, err := p.encodeList(vals)
	if err != nil {
		return err
	}
	resp, interrupted, err := p.raw.ListAppendOp(ctx, l.h.id, enc)
	if err != nil {
		return err
	}
	return p.decodeEmptyResult(ctx, resp, interrupted)
}

// Values returns every element, in order.
func (l ListValue) Values(ctx context.Context) ([]Value, error) { return l.values(ctx) }

// ---- TupleValue (tuple) ------------------------------------------------------

// TupleValue is a tuple (or a tuple subclass, e.g. a namedtuple).
type TupleValue struct{ object }

// Kind reports KindTuple.
func (TupleValue) Kind() Kind { return KindTuple }

// Index is t[i].
func (t TupleValue) Index(ctx context.Context, i int) (Value, error) { return t.index(ctx, i) }

// Values returns every element, in order.
func (t TupleValue) Values(ctx context.Context) ([]Value, error) { return t.values(ctx) }

// ---- DictValue (dict) --------------------------------------------------------

// DictItem is one key/value pair of a dict (NewDict input, Items output).
type DictItem struct {
	Key   Value
	Value Value
}

// DictValue is a dict (or a dict subclass, e.g. an OrderedDict).
type DictValue struct{ object }

// Kind reports KindDict.
func (DictValue) Kind() Kind { return KindDict }

// Get is d[key] with the KeyError folded into the boolean: the value and
// true when the key is present, None and false when it is not.
func (d DictValue) Get(ctx context.Context, key Value) (Value, bool, error) {
	p, err := d.raw()
	if err != nil {
		return nil, false, err
	}
	k, err := p.encodeSingle(key)
	if err != nil {
		return nil, false, err
	}
	resp, interrupted, err := p.raw.DictGetOp(ctx, d.h.id, k)
	if err != nil {
		return nil, false, err
	}
	return p.decodeExistsNodeResult(ctx, resp, interrupted)
}

// Set is d[key] = v.
func (d DictValue) Set(ctx context.Context, key, v Value) error { return d.SetItem(ctx, key, v) }

// Delete is del d[key].
func (d DictValue) Delete(ctx context.Context, key Value) error { return d.DelItem(ctx, key) }

// Keys is list(d.keys()).
func (d DictValue) Keys(ctx context.Context) ([]Value, error) {
	p, err := d.raw()
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.DictKeysOp(ctx, d.h.id)
	if err != nil {
		return nil, err
	}
	return p.decodeListResult(ctx, resp, interrupted)
}

// Values is list(d.values()).
func (d DictValue) Values(ctx context.Context) ([]Value, error) {
	p, err := d.raw()
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.DictValuesOp(ctx, d.h.id)
	if err != nil {
		return nil, err
	}
	return p.decodeListResult(ctx, resp, interrupted)
}

// Items is list(d.items()).
func (d DictValue) Items(ctx context.Context) ([]DictItem, error) {
	p, err := d.raw()
	if err != nil {
		return nil, err
	}
	resp, interrupted, err := p.raw.DictItemsOp(ctx, d.h.id)
	if err != nil {
		return nil, err
	}
	flat, err := p.decodeListResult(ctx, resp, interrupted)
	if err != nil {
		return nil, err
	}
	if len(flat)%2 != 0 {
		return nil, fmt.Errorf("python: malformed dict items result")
	}
	items := make([]DictItem, 0, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		items = append(items, DictItem{Key: flat[i], Value: flat[i+1]})
	}
	return items, nil
}

// ---- SetValue (set) / FrozenSetValue (frozenset) -----------------------------

// SetValue is a set.
type SetValue struct{ object }

// Kind reports KindSet.
func (SetValue) Kind() Kind { return KindSet }

// Add is s.add(v).
func (s SetValue) Add(ctx context.Context, v Value) error {
	p, err := s.raw()
	if err != nil {
		return err
	}
	val, err := p.encodeSingle(v)
	if err != nil {
		return err
	}
	resp, interrupted, err := p.raw.SetAddOp(ctx, s.h.id, val)
	if err != nil {
		return err
	}
	return p.decodeEmptyResult(ctx, resp, interrupted)
}

// Discard is s.discard(v): removes v when present, no error when absent.
func (s SetValue) Discard(ctx context.Context, v Value) error {
	p, err := s.raw()
	if err != nil {
		return err
	}
	val, err := p.encodeSingle(v)
	if err != nil {
		return err
	}
	resp, interrupted, err := p.raw.SetDiscardOp(ctx, s.h.id, val)
	if err != nil {
		return err
	}
	return p.decodeEmptyResult(ctx, resp, interrupted)
}

// Values returns every element (in the set's iteration order).
func (s SetValue) Values(ctx context.Context) ([]Value, error) { return s.values(ctx) }

// FrozenSetValue is a frozenset.
type FrozenSetValue struct{ object }

// Kind reports KindFrozenSet.
func (FrozenSetValue) Kind() Kind { return KindFrozenSet }

// Values returns every element (in the set's iteration order).
func (s FrozenSetValue) Values(ctx context.Context) ([]Value, error) { return s.values(ctx) }

// ---- FunctionValue / ClassValue / ModuleValue --------------------------------

// FunctionValue is a routine: a function defined in Python, a builtin
// function (including one made by NewFunction), a bound method, or a method
// descriptor. Invoke it with Call.
type FunctionValue struct{ object }

// Kind reports KindFunction.
func (FunctionValue) Kind() Kind { return KindFunction }

// ClassValue is a class. Instantiate it with Call.
type ClassValue struct{ object }

// Kind reports KindClass.
func (ClassValue) Kind() Kind { return KindClass }

// ModuleValue is a module: what Import returns.
type ModuleValue struct{ object }

// Kind reports KindModule.
func (ModuleValue) Kind() Kind { return KindModule }
