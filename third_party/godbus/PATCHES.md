# godbus patches

This is a vendored copy of [github.com/godbus/dbus][upstream] (v5.2.2 + main
as of clone, see `.go-mod`/`go.mod`). It is here only because we need a single
small fix that is not yet upstream. Re-sync from upstream with:

```
cd third_party && rm -rf godbus && \
  git clone --depth 1 https://github.com/godbus/dbus.git godbus && \
  rm -rf godbus/.git
```

then re-apply the patch in `decoder.go` below and run the test suite from the
parent module.

---

## Patch: `decoder.go` — coerce `UnixFD` → `UnixFDIndex` when appending into a `[]UnixFDIndex` slice

### Symptom

A method whose signature includes `a(sv)` (or any nested `ah`) where the
sender passes real file descriptors panics inside the variant decoder. The
panic is `recover()`'d by `(dec *decoder).Decode`, but the recover only catches
`error`-typed panics — `reflect.Append`'s panic value is a string, so it is
swallowed silently. The decoder returns partial args; the export-side
`standardMethodArgumentDecode` sees fewer body values than expected and
returns `org.freedesktop.DBus.Error.InvalidArgs: "Invalid type / number of
args"` to the caller before the handler runs.

This hits us because systemd 255's `systemd-run --user --scope` always sends
`PIDFDs` (signature `ah`) when the bus negotiated `UNIX_FD`, which it does by
default on a Unix socket transport.

### Root cause

In `decoder.go`, the `case 'a'`:

```go
v := reflect.MakeSlice(reflect.SliceOf(typeFor(s[1:])), 0, capacity)
…
for dec.pos < spos+int(length) {
    ev := dec.decode(s[1:], depth+1)
    v = reflect.Append(v, reflect.ValueOf(ev))
}
```

`typeFor("h")` returns `unixFDIndexType` (Go type `UnixFDIndex`, `uint32`).
But `case 'h'` returns one of two different types depending on whether the
message carries fds:

```go
case 'h':
    idx := dec.decodeU()
    if int(idx) < len(dec.fds) {
        return UnixFD(dec.fds[idx])   // Go type UnixFD (int32)
    }
    return UnixFDIndex(idx)            // Go type UnixFDIndex (uint32)
```

When fds are attached the element value is `UnixFD` but the slice's element
type is `UnixFDIndex` — `reflect.Append` rejects this and panics.

### Fix

Coerce when convertible:

```go
elemType := v.Type().Elem()
for dec.pos < spos+int(length) {
    ev := dec.decode(s[1:], depth+1)
    rv := reflect.ValueOf(ev)
    if rv.Type() != elemType && rv.Type().ConvertibleTo(elemType) {
        rv = rv.Convert(elemType)
    }
    v = reflect.Append(v, rv)
}
```

`UnixFD`/`UnixFDIndex` are both `int32`/`uint32` aliases so `ConvertibleTo`
holds. No other types should hit this path in practice — the only place where
`case 'X'` returns a type that differs from `typeFor("X")` is `case 'h'`.

[upstream]: https://github.com/godbus/dbus
