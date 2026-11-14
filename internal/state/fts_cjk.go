package state

import (
	"fmt"
	"log/slog"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Name the CJK bigram tokenizer registers under
const cjkTokenizerName = "cjk_unicode61"

// Reports whether a code point is Hangul, a CJK ideograph, Hiragana or Katakana
func cjkCodePoint(cp uint32) bool {
	return (cp >= 0xAC00 && cp <= 0xD7A3) || (cp >= 0x1100 && cp <= 0x11FF) || (cp >= 0x3130 && cp <= 0x318F) ||
		(cp >= 0xA960 && cp <= 0xA97F) || (cp >= 0xD7B0 && cp <= 0xD7FF) || (cp >= 0x4E00 && cp <= 0x9FFF) ||
		(cp >= 0x3400 && cp <= 0x4DBF) || (cp >= 0xF900 && cp <= 0xFAFF) || (cp >= 0x20000 && cp <= 0x2FA1F) ||
		(cp >= 0x3040 && cp <= 0x309F) || (cp >= 0x30A0 && cp <= 0x30FF) || (cp >= 0x31F0 && cp <= 0x31FF)
}

// Length and code point of the UTF-8 sequence at the start of p, an invalid lead byte decoding as itself
func cjkDecode(p []byte) (int, uint32) {
	c, n := uint32(p[0]), len(p)
	switch {
	case c < 0x80:
		return 1, c
	case c&0xE0 == 0xC0 && n >= 2:
		return 2, (c&0x1F)<<6 | uint32(p[1]&0x3F)
	case c&0xF0 == 0xE0 && n >= 3:
		return 3, (c&0x0F)<<12 | uint32(p[1]&0x3F)<<6 | uint32(p[2]&0x3F)
	case c&0xF8 == 0xF0 && n >= 4:
		return 4, (c&0x07)<<18 | uint32(p[1]&0x3F)<<12 | uint32(p[2]&0x3F)<<6 | uint32(p[3]&0x3F)
	}
	return 1, c
}

// Byte span of the first CJK code point of p at or after from, both ends len(p) when none follows
func cjkNext(p []byte, from int) (int, int, uint32) {
	for from < len(p) {
		l, cp := cjkDecode(p[from:])
		if cjkCodePoint(cp) {
			return from, from + l, cp
		}
		from += l
	}
	return len(p), len(p), 0
}

// Reports whether token holds CJK, and whether source carries the same CJK code points in order
func cjkCompare(token, source []byte) (bool, bool) {
	at, next, want := cjkNext(token, 0)
	if at == len(token) {
		return false, false
	}
	if source == nil {
		return true, false
	}
	o := 0
	for at < len(token) {
		from, to, got := cjkNext(source, o)
		if from == len(source) || got != want {
			return true, false
		}
		o = to
		at, next, want = cjkNext(token, next)
	}
	rest, _, _ := cjkNext(source, o)
	return true, rest == len(source)
}

// Splits one folded unicode61 token into CJK bigrams, lone CJK unigrams and untouched other text, offsets naming source bytes
func cjkSplit(token, source []byte, start, end int32, emit func(off, n int, start, end int32) int32) int32 {
	hasCJK, exact := cjkCompare(token, source)
	if !hasCJK {
		return emit(0, len(token), start, end)
	}
	clamp := func(v int) int32 { return min(start+int32(v), end) }
	at := func(v int) int32 { return start + int32(v) }
	rc, o := int32(sqlite3.SQLITE_OK), 0
	for i := 0; i < len(token) && rc == sqlite3.SQLITE_OK; {
		seg := i
		l, cp := cjkDecode(token[i:])
		if !cjkCodePoint(cp) {
			i, _, _ = cjkNext(token, i+l)
			if !exact {
				rc = emit(seg, i-seg, clamp(seg), clamp(i))
				continue
			}
			from := o
			o, _, _ = cjkNext(source, o)
			rc = emit(seg, i-seg, at(from), at(o))
			continue
		}
		// One CJK run, each code point paired with its predecessor, the source span of every code point found in step
		firstStart, firstEnd := 0, 0
		if exact {
			firstStart, firstEnd, _ = cjkNext(source, o)
			o = firstEnd
		}
		prevStart := firstStart
		b0, b1 := seg, seg+l
		i += l
		chars := 1
		for i < len(token) {
			l2, cp2 := cjkDecode(token[i:])
			if !cjkCodePoint(cp2) {
				break
			}
			i += l2
			chars++
			s, e := clamp(b0), clamp(i)
			if exact {
				curStart, curEnd, _ := cjkNext(source, o)
				s, e, prevStart, o = at(prevStart), at(curEnd), curStart, curEnd
			}
			if rc = emit(b0, i-b0, s, e); rc != sqlite3.SQLITE_OK {
				break
			}
			b0, b1 = b1, i
		}
		if rc == sqlite3.SQLITE_OK && chars == 1 {
			s, e := clamp(seg), clamp(b1)
			if exact {
				s, e = at(firstStart), at(firstEnd)
			}
			rc = emit(seg, b1-seg, s, e)
		}
	}
	return rc
}

// Tokenizer instance in SQLite memory, the unicode61 methods and instance it wraps
type cjkInstance struct {
	inner  sqlite3.Tfts5_tokenizer
	pInner uintptr
}

// Token sink of one tokenize call, the caller context and callback it forwards to and the text being tokenized
type cjkSink struct {
	ctx   uintptr
	token uintptr
	text  uintptr
	size  int32
}

// Signatures of the FTS5 tokenizer and API callbacks as the transpiled library calls them
type (
	cjkTokenFn    = func(*libc.TLS, uintptr, int32, uintptr, int32, int32, int32) int32
	cjkCreateFn   = func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	cjkDeleteFn   = func(*libc.TLS, uintptr)
	cjkTokenizeFn = func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32
	cjkFindFn     = func(*libc.TLS, uintptr, uintptr, uintptr, uintptr) int32
	cjkRegisterFn = func(*libc.TLS, uintptr, uintptr, uintptr, uintptr, uintptr) int32
)

// Memory SQLite or the libc stack handed out, addressed as a Go pointer
func cmem(p uintptr) unsafe.Pointer { return unsafe.Add(unsafe.Pointer(nil), p) }

// C function pointer of a top level Go function, the funcval word the transpiled code calls through
func cfuncPtr[T any](f T) uintptr { return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f})) }

// Go function behind a C function pointer
func cfunc[T any](fp uintptr) T { return *(*T)(unsafe.Pointer(&struct{ p uintptr }{fp})) }

// NUL terminated copy of s on the libc stack, released by the matching Free of len(s)+1
func tlsString(tls *libc.TLS, s string) uintptr {
	p := tls.Alloc(len(s) + 1)
	buf := unsafe.Slice((*byte)(cmem(p)), len(s)+1)
	copy(buf, s)
	buf[len(s)] = 0
	return p
}

// Deferred by every callback SQLite enters, turning a panic of the tokenizer's own code into an SQLite error
func cjkContain(rc *int32, inSQLite *bool, callback string) {
	if *inSQLite {
		return
	}
	if r := recover(); r != nil {
		slog.Error(fmt.Sprintf("fts5_cjk %s callback failed inside SQLite and was turned into an SQL error: %v", callback, r))
		*rc = sqlite3.SQLITE_ERROR
	}
}

// Forwards each unicode61 token through the bigram splitter to the caller sink
func cjkInnerToken(tls *libc.TLS, pCtx uintptr, flags int32, pToken uintptr, nToken, start, end int32) (rc int32) {
	inSQLite := false
	defer cjkContain(&rc, &inSQLite, "token")
	sink := (*cjkSink)(cmem(pCtx))
	if sink.token == 0 {
		return sqlite3.SQLITE_ERROR
	}
	outer := cfunc[cjkTokenFn](sink.token)
	var token, source []byte
	if nToken > 0 {
		token = unsafe.Slice((*byte)(cmem(pToken)), nToken)
	}
	if sink.text != 0 && start >= 0 && start < end && end <= sink.size {
		source = unsafe.Slice((*byte)(cmem(sink.text+uintptr(start))), end-start)
	}
	return cjkSplit(token, source, start, end, func(off, n int, s, e int32) int32 {
		inSQLite = true
		r := outer(tls, sink.ctx, flags, pToken+uintptr(off), int32(n), s, e)
		inSQLite = false
		return r
	})
}

// Creates an instance wrapping a unicode61 tokenizer built from the same arguments
func cjkCreate(tls *libc.TLS, pAPI, azArg uintptr, nArg int32, ppOut uintptr) (rc int32) {
	inSQLite := false
	defer cjkContain(&rc, &inSQLite, "create")
	find := (*sqlite3.Tfts5_api)(cmem(pAPI)).FxFindTokenizer
	if find == 0 || ppOut == 0 {
		return sqlite3.SQLITE_ERROR
	}
	word := int(unsafe.Sizeof(uintptr(0)))
	slot := tls.Alloc(word)
	defer tls.Free(word)
	*(*uintptr)(cmem(slot)) = 0
	name := tlsString(tls, "unicode61")
	defer tls.Free(len("unicode61") + 1)
	inSQLite = true
	p := sqlite3.Xsqlite3_malloc(tls, int32(unsafe.Sizeof(cjkInstance{})))
	inSQLite = false
	if p == 0 {
		return sqlite3.SQLITE_NOMEM
	}
	inst := (*cjkInstance)(cmem(p))
	*inst = cjkInstance{}
	inSQLite = true
	rc = cfunc[cjkFindFn](find)(tls, pAPI, name, slot, p)
	inSQLite = false
	if rc == sqlite3.SQLITE_OK && (inst.inner.FxCreate == 0 || inst.inner.FxDelete == 0 || inst.inner.FxTokenize == 0) {
		rc = sqlite3.SQLITE_ERROR
	}
	inSQLite = true
	if rc == sqlite3.SQLITE_OK {
		rc = cfunc[cjkCreateFn](inst.inner.FxCreate)(tls, *(*uintptr)(cmem(slot)), azArg, nArg, p+unsafe.Offsetof(inst.pInner))
	}
	if rc != sqlite3.SQLITE_OK {
		sqlite3.Xsqlite3_free(tls, p)
		return rc
	}
	inSQLite = false
	*(*uintptr)(cmem(ppOut)) = p
	return sqlite3.SQLITE_OK
}

// Deletes the wrapped unicode61 instance and frees the wrapper
func cjkDelete(tls *libc.TLS, pTok uintptr) {
	var rc int32
	inSQLite := false
	defer cjkContain(&rc, &inSQLite, "delete")
	if pTok == 0 {
		return
	}
	inst := (*cjkInstance)(cmem(pTok))
	inSQLite = true
	if inst.pInner != 0 {
		cfunc[cjkDeleteFn](inst.inner.FxDelete)(tls, inst.pInner)
	}
	sqlite3.Xsqlite3_free(tls, pTok)
}

// Tokenizes through unicode61 with every emitted token passed through the bigram splitter
func cjkTokenize(tls *libc.TLS, pTok, pCtx uintptr, flags int32, pText uintptr, nText int32, xToken uintptr) (rc int32) {
	inSQLite := false
	defer cjkContain(&rc, &inSQLite, "tokenize")
	inst := (*cjkInstance)(cmem(pTok))
	size := int(unsafe.Sizeof(cjkSink{}))
	sink := tls.Alloc(size)
	defer tls.Free(size)
	*(*cjkSink)(cmem(sink)) = cjkSink{ctx: pCtx, token: xToken, text: pText, size: nText}
	tokenize := cfunc[cjkTokenizeFn](inst.inner.FxTokenize)
	inSQLite = true
	return tokenize(tls, inst.pInner, sink, flags, pText, nText, cfuncPtr(cjkInnerToken))
}

// The fts5_api of a connection through SELECT fts5(?1), zero when FTS5 is absent
func fts5API(tls *libc.TLS, db uintptr) uintptr {
	word := int(unsafe.Sizeof(uintptr(0)))
	slots := tls.Alloc(2 * word)
	defer tls.Free(2 * word)
	pRet, ppStmt := slots, slots+uintptr(word)
	*(*uintptr)(cmem(pRet)), *(*uintptr)(cmem(ppStmt)) = 0, 0
	query := tlsString(tls, "SELECT fts5(?1)")
	defer tls.Free(len("SELECT fts5(?1)") + 1)
	kind := tlsString(tls, "fts5_api_ptr")
	defer tls.Free(len("fts5_api_ptr") + 1)
	if sqlite3.Xsqlite3_prepare_v2(tls, db, query, -1, ppStmt, 0) == sqlite3.SQLITE_OK {
		stmt := *(*uintptr)(cmem(ppStmt))
		sqlite3.Xsqlite3_bind_pointer(tls, stmt, 1, pRet, kind, 0)
		sqlite3.Xsqlite3_step(tls, stmt)
	}
	sqlite3.Xsqlite3_finalize(tls, *(*uintptr)(cmem(ppStmt)))
	return *(*uintptr)(cmem(pRet))
}

// Registers the CJK bigram tokenizer on the connection c runs on
func registerCJKTokenizer(c Conn) error {
	r, ok := c.(*runner)
	if !ok {
		return callErr(nil, "fts5_cjk: %T exposes no SQLite handle", c)
	}
	return r.c.raw(func(tls *libc.TLS, db uintptr) error {
		api := fts5API(tls, db)
		if api == 0 {
			return callErr(nil, "fts5_cjk: FTS5 unavailable")
		}
		size := int(unsafe.Sizeof(sqlite3.Tfts5_tokenizer{}))
		tok := tls.Alloc(size)
		defer tls.Free(size)
		*(*sqlite3.Tfts5_tokenizer)(cmem(tok)) = sqlite3.Tfts5_tokenizer{
			FxCreate: cfuncPtr(cjkCreate), FxDelete: cfuncPtr(cjkDelete), FxTokenize: cfuncPtr(cjkTokenize),
		}
		name := tlsString(tls, cjkTokenizerName)
		defer tls.Free(len(cjkTokenizerName) + 1)
		register := cfunc[cjkRegisterFn]((*sqlite3.Tfts5_api)(cmem(api)).FxCreateTokenizer)
		if rc := register(tls, api, name, api, tok, 0); rc != sqlite3.SQLITE_OK {
			return callErr(nil, "fts5_cjk: registering %s returned %d", cjkTokenizerName, rc)
		}
		return nil
	})
}
