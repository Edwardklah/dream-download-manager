# Go lesson 1: the Go you need to read stage 1

This covers every Go feature used in stage 1, compared with Python where that helps.
Read it alongside `engine/download.go`. The code has `GO NOTE` comments at the
first place each idea appears.

## 1. Projects, modules and packages

- A **module** is a whole project. `go.mod` names it (`module dlmanager`) and sets
  the Go version. It's the equivalent of `package.json` in your Next.js projects.
- A **package** is one folder of `.go` files. Every file in `engine/` starts with
  `package engine`, and they all share their functions and types freely; there's
  no need to import one file from another.
- To use a package from elsewhere, import it by module name + folder:
  `import "dlmanager/engine"`, then refer to things as `engine.Download`.
- A program needs `package main` with a `func main()`. That's `cmd/dl/main.go`.
  Putting programs under `cmd/<name>/` is a Go convention; later the desktop app
  will be a second program using the same `engine` package.

## 2. Capital letters mean public

There are no `public`/`private` keywords. A name that starts with a capital letter
(`Download`, `Probe`, `StatusFailed`) can be used from other packages. A lowercase
name (`newClient`, `speedMeter`) is private to its package.

That's how `engine` hides its internals: `cmd/dl` can call `engine.Probe` but not
`engine.pickFilename`.

## 3. Variables and types

Go is statically typed: every variable has a fixed type, checked when you compile.

```go
var downloaded int64      // declare with a type; starts at its "zero value" (0)
name := "download"        // := declares AND assigns; Go works out the type (string)
name = "video.mp4"        // plain = assigns to a variable that already exists
```

Zero values matter: an `int` starts at `0`, a `string` at `""`, a `bool` at
`false`, a pointer at `nil` (Go's `None`). Variables are never "undefined".

Types used in stage 1:

| Go | Python equivalent | Notes |
|---|---|---|
| `int64` | `int` | 64-bit integer. Used for byte counts because files exceed 2 GB |
| `float64` | `float` | |
| `string` | `str` | |
| `bool` | `bool` | |
| `[]byte` | `bytes`/`bytearray` | a **slice** of bytes (see below) |
| `map[string]string` | `dict[str, str]` | |
| `time.Duration` | `timedelta` | `100 * time.Millisecond`, `3 * time.Second` |

Go never converts number types automatically. Adding an `int` to an `int64` is a
compile error; you write `downloaded += int64(n)`. Annoying at first, but it
prevents a whole category of bugs.

## 4. Structs and methods

A **struct** groups named fields, like a Python dataclass:

```go
type Progress struct {
    Downloaded int64
    Total      int64
}
```

Create one with field names: `Progress{Downloaded: 10, Total: 100}`. Fields you
leave out get their zero value.

A **method** is a function attached to a type. The part in brackets before the
name is the **receiver**, Go's version of `self`:

```go
func (p Progress) Percent() float64 {
    return float64(p.Downloaded) / float64(p.Total) * 100
}
```

Called as `p.Percent()`, exactly like Python.

## 5. Pointers: `*` and `&`

This is the main idea Python doesn't make you think about.

When you pass a struct to a function in Go, the function gets a **copy**. Changes
to the copy don't affect the original. A **pointer** is the address of the original,
so changes made through it stick.

- `&x` means "the address of x" (make a pointer to it).
- `*Download` as a type means "a pointer to a Download".

```go
dl := &engine.Download{URL: url}   // dl is a pointer to a new Download
dl.Run(ctx)                        // Run can now fill in dl.Info
```

`Run` is declared as `func (d *Download) Run(...)` (pointer receiver) because it
sets `d.Info` after probing. With `(d Download)` it would set `Info` on a copy, and
`cmd/dl` would never see it.

Rule of thumb: use a pointer receiver when the method changes the struct, or when
the struct is large. Go automatically handles `.` on pointers, so you never write
`(*d).Info`.

## 6. Errors are values, not exceptions

Go has no `try`/`except`. A function that can fail returns an `error` as its last
result, and the caller checks it immediately:

```go
info, err := Probe(ctx, client, url, headers)
if err != nil {
    return "", err   // pass it up to whoever called us
}
// from here on, info is safe to use
```

`nil` means "no error". You'll write `if err != nil` constantly. It feels
repetitive, but every failure point is visible in the code; nothing is thrown from
somewhere invisible.

Adding context to an error:

```go
return fmt.Errorf("%w: got %d of %d bytes", ErrIncomplete, downloaded, size)
```

`%w` wraps `ErrIncomplete` inside the new message. Code further up can still ask
`errors.Is(err, engine.ErrIncomplete)` and get `true`. That's how the CLI knows to
print "Incomplete download" instead of a generic failure.

## 7. `defer`: cleanup that always runs

`defer` schedules a call for when the current function returns, however it returns.

```go
resp, err := client.Do(req)
if err != nil { return nil, err }
defer resp.Body.Close()   // the connection is closed when Probe returns
```

It's the Go version of `with open(...) as f:`. Deferred calls run in reverse order.

`Run` uses a smarter version: a deferred function that looks at the returned `err`
and reports "cancelled" or "failed" to the interface. Because it's in one place,
every failure path in `Run` gets reported, and we can't forget one.

## 8. Context: how cancellation works

`context.Context` is passed into anything that might take a long time. When the
context is cancelled, network calls using it stop and return an error.

In `cmd/dl/main.go`:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
```

This creates a context that is cancelled when you press Ctrl+C. It's passed to
`dl.Run(ctx)`, which passes it to every request. Press Ctrl+C, and the current
`Read` fails; `Run` sees `ctx.Err() != nil`, reports "cancelled", and leaves the
`.part` file in place.

In stage 5, the Cancel button will cancel a context in exactly the same way.

## 9. Loops and switch

Go has one loop keyword, `for`, used in three forms:

```go
for {                          // forever (until break or return)
for n := 1; n < 10; n++ {      // classic counter loop
for key, value := range m {    // over a map, slice or string
```

`switch` doesn't fall through to the next case, so no `break` is needed.
`switch { case a > b: ... }` with no value works like an `if / else if` chain.

## 10. Slices

A slice (`[]byte`, `[]string`) is a resizable list. `make([]byte, 64*1024)`
creates one of 64 KB.

`buf[:n]` means "the first n items". When we read from the network,
`resp.Body.Read(buf)` fills the buffer and tells us how many bytes it got (`n`).
We write only `buf[:n]`, since the rest of the buffer may hold old data.

`append(s, x)` adds to a slice and returns the new slice. You must reassign it:
`m.samples = append(m.samples, sample{...})`.

## How the download loop works

Putting it together, the heart of `Run`:

```go
for {
    n, readErr := resp.Body.Read(buf)   // wait for up to 64 KB from the network
    if n > 0 {
        file.Write(buf[:n])              // save what arrived
        downloaded += int64(n)           // count it
        // ...report progress at most 10 times a second
    }
    if readErr == io.EOF { break }      // EOF = the server finished sending
    if readErr != nil { return ... }    // connection broke, or Ctrl+C
}
```

Note the order: `Read` can return data AND an error in the same call, so we save
the data before checking the error. Getting this backwards silently loses the last
chunk of a file. This is a classic Go mistake.

## Exercises

Try these before stage 2. They're small and touch the parts you've just read.

1. In `cmd/dl/main.go`, change the progress bar width from 30 to 40 characters.
   (Hint: there are two places.)
2. Make the CLI print how long the download took, e.g. `Took 00:12`.
   (Hint: `start := time.Now()` before `dl.Run`, then `time.Since(start)` after.
   `humanTime` already formats a `time.Duration`.)
3. Change `reportInterval` in `download.go` to `500 * time.Millisecond` and run a
   download. What changes, and why might a slower rate be better for a GUI?
4. Run `go vet ./...` and `gofmt -l .` in the project folder. They check for bugs
   and formatting. Both should print nothing.
