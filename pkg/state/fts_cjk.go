package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const FTSCJKTableSQL = `
CREATE VIEW IF NOT EXISTS messages_fts_cjk_src AS
    SELECT id, role, content, tool_name, tool_calls
    FROM messages
    WHERE role <> 'tool';

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts_cjk USING fts5(
    content,
    tool_name,
    tool_calls,
    content='messages_fts_cjk_src',
    content_rowid='id',
    tokenize='cjk_unicode61'
);
`

const FTSCJKTriggerSQL = `
CREATE TRIGGER IF NOT EXISTS messages_fts_cjk_insert AFTER INSERT ON messages
WHEN new.role <> 'tool'
   AND (new.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta
                           WHERE key = 'fts_cjk_rebuild_high_water'), -1)
     OR new.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta
                            WHERE key = 'fts_cjk_rebuild_progress'), -1))
BEGIN
    INSERT INTO messages_fts_cjk(rowid, content, tool_name, tool_calls)
    VALUES (new.id, new.content, new.tool_name, new.tool_calls);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_cjk_delete AFTER DELETE ON messages
WHEN old.role <> 'tool'
   AND (old.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta
                           WHERE key = 'fts_cjk_rebuild_high_water'), -1)
     OR old.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta
                            WHERE key = 'fts_cjk_rebuild_progress'), -1))
BEGIN
    INSERT INTO messages_fts_cjk(messages_fts_cjk, rowid, content, tool_name, tool_calls)
    VALUES ('delete', old.id, old.content, old.tool_name, old.tool_calls);
END;

CREATE TRIGGER IF NOT EXISTS messages_fts_cjk_update
AFTER UPDATE OF content, tool_name, tool_calls, role ON messages
WHEN (old.content IS NOT new.content
    OR old.tool_name IS NOT new.tool_name
    OR old.tool_calls IS NOT new.tool_calls
    OR old.role IS NOT new.role)
   AND (old.id > COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta
                           WHERE key = 'fts_cjk_rebuild_high_water'), -1)
     OR old.id <= COALESCE((SELECT CAST(value AS INTEGER) FROM state_meta
                            WHERE key = 'fts_cjk_rebuild_progress'), -1))
BEGIN
    INSERT INTO messages_fts_cjk(messages_fts_cjk, rowid, content, tool_name, tool_calls)
    SELECT 'delete', old.id, old.content, old.tool_name, old.tool_calls
    WHERE old.role <> 'tool';
    INSERT INTO messages_fts_cjk(rowid, content, tool_name, tool_calls)
    SELECT new.id, new.content, new.tool_name, new.tool_calls
    WHERE new.role <> 'tool';
END;
`

// Location of the cjk_unicode61 extension, ICEBATH_FTS5_CJK_SO or the home lib directory
func FTS5CJKSoPath(ctx context.Context) string {
	if env := os.Getenv("ICEBATH_FTS5_CJK_SO"); env != "" {
		return pypath.Expanduser(env)
	}
	return filepath.Join(home.Dir(ctx), "lib", "libfts5_cjk.so")
}

// Reports whether sessions.cjk_fts, default on, allows the CJK bigram index for the served profile
func cjkFTSConfigEnabled(ctx context.Context) bool {
	value := RoutedSessionsSetting(ctx, "cjk_fts", "ICEBATH_CJK_FTS")
	if value == nil {
		return true
	}
	text, err := pyrepr.Format(value, "")
	if err != nil {
		text = pyrepr.Repr(value)
	}
	return !slices.Contains([]string{"0", "false", "off", "no"}, pystr.Lower(pystr.Strip(text)))
}

// Registers the built in cjk_unicode61 tokenizer on conn unless sessions.cjk_fts is off or an explicit extension path is absent
var LoadFTS5CJKExtension = func(ctx context.Context, conn SQLiteConn) bool {
	path := FTS5CJKSoPath(ctx)
	if !cjkFTSConfigEnabled(ctx) {
		return false
	}
	if os.Getenv("ICEBATH_FTS5_CJK_SO") != "" {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	if err := withRawSQLite(conn, registerCJKTokenizer); err != nil {
		slog.Warn(fmt.Sprintf("fts5_cjk extension load failed (%s)", path), "error", err)
		return false
	}
	return true
}

// Reports whether a codepoint is Hangul, a CJK ideograph, Hiragana or Katakana
func cjkIsCJK(cp uint32) bool {
	return (cp >= 0xAC00 && cp <= 0xD7A3) ||
		(cp >= 0x1100 && cp <= 0x11FF) ||
		(cp >= 0x3130 && cp <= 0x318F) ||
		(cp >= 0xA960 && cp <= 0xA97F) ||
		(cp >= 0xD7B0 && cp <= 0xD7FF) ||
		(cp >= 0x4E00 && cp <= 0x9FFF) ||
		(cp >= 0x3400 && cp <= 0x4DBF) ||
		(cp >= 0xF900 && cp <= 0xFAFF) ||
		(cp >= 0x20000 && cp <= 0x2FA1F) ||
		(cp >= 0x3040 && cp <= 0x309F) ||
		(cp >= 0x30A0 && cp <= 0x30FF) ||
		(cp >= 0x31F0 && cp <= 0x31FF)
}

// Decodes one UTF-8 codepoint without validation, an invalid lead byte decoding as itself over one byte
func cjkUTF8Decode(p []byte) (int, uint32) {
	c := uint32(p[0])
	n := len(p)
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

// Re-emits one unicode61 token with CJK runs as overlapping bigrams and a lone CJK char as a unigram
func cjkEmit(token []byte, iStart, iEnd int32, emit func(off, n int, start, end int32) int32) int32 {
	hasCJK := false
	for i := 0; i < len(token); {
		l, cp := cjkUTF8Decode(token[i:])
		if cjkIsCJK(cp) {
			hasCJK = true
			break
		}
		i += l
	}
	if !hasCJK {
		return emit(0, len(token), iStart, iEnd)
	}
	clamp := func(v int) int32 { return min(iStart+int32(v), iEnd) }
	rc := int32(sqlite3.SQLITE_OK)
	for i := 0; i < len(token) && rc == sqlite3.SQLITE_OK; {
		segStart := i
		l, cp := cjkUTF8Decode(token[i:])
		if !cjkIsCJK(cp) {
			i += l
			for i < len(token) {
				l2, cp2 := cjkUTF8Decode(token[i:])
				if cjkIsCJK(cp2) {
					break
				}
				i += l2
			}
			rc = emit(segStart, i-segStart, clamp(segStart), clamp(i))
			continue
		}
		b0, b1 := segStart, segStart+l
		i += l
		chars := 1
		for i < len(token) {
			l2, cp2 := cjkUTF8Decode(token[i:])
			if !cjkIsCJK(cp2) {
				break
			}
			i += l2
			chars++
			b2 := i
			rc = emit(b0, b2-b0, clamp(b0), clamp(b2))
			if rc != sqlite3.SQLITE_OK {
				break
			}
			b0, b1 = b1, b2
		}
		if rc == sqlite3.SQLITE_OK && chars == 1 {
			rc = emit(segStart, b1-segStart, clamp(segStart), clamp(b1))
		}
	}
	return rc
}

// CjkTokenizer of fts5_cjk.c, the unicode61 methods and instance it wraps
type cjkTokenizer struct {
	inner  sqlite3.Tfts5_tokenizer
	pInner uintptr
}

// CjkCallbackCtx of fts5_cjk.c, the caller token sink a tokenize call forwards to
type cjkCallbackCtx struct {
	pOuterCtx   uintptr
	xOuterToken uintptr
}

// Address of non Go memory the SQLite allocator or libc stack handed out
func cMem(p uintptr) unsafe.Pointer { return unsafe.Add(unsafe.Pointer(nil), p) }

// C pointer of a top level function, the representation ccgo code calls through
func cFuncPtr[T any](f T) uintptr { return *(*uintptr)(unsafe.Pointer(&struct{ f T }{f})) }

// Go function behind a C function pointer of type T
func cFunc[T any](fp uintptr) T { return *(*T)(unsafe.Pointer(&struct{ uintptr }{fp})) }

type (
	fts5TokenFn        = func(*libc.TLS, uintptr, int32, uintptr, int32, int32, int32) int32
	fts5CreateFn       = func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	fts5DeleteFn       = func(*libc.TLS, uintptr)
	fts5TokenizeFn     = func(*libc.TLS, uintptr, uintptr, int32, uintptr, int32, uintptr) int32
	fts5FindTokenizer  = func(*libc.TLS, uintptr, uintptr, uintptr, uintptr) int32
	fts5CreateTokenize = func(*libc.TLS, uintptr, uintptr, uintptr, uintptr, uintptr) int32
)

// Copies s into libc stack memory as a NUL terminated string, released by the matching tls.Free
func tlsCString(tls *libc.TLS, s string) uintptr {
	p := tls.Alloc(len(s) + 1)
	buf := unsafe.Slice((*byte)(cMem(p)), len(s)+1)
	copy(buf, s)
	buf[len(s)] = 0
	return p
}

// Forwards each unicode61 token through the CJK bigram splitter to the caller sink
func cjkInnerCallback(tls *libc.TLS, pCtx uintptr, tflags int32, pToken uintptr, nToken, iStart, iEnd int32) int32 {
	cb := (*cjkCallbackCtx)(cMem(pCtx))
	outer := cFunc[fts5TokenFn](cb.xOuterToken)
	var token []byte
	if nToken > 0 {
		token = unsafe.Slice((*byte)(cMem(pToken)), nToken)
	}
	return cjkEmit(token, iStart, iEnd, func(off, n int, start, end int32) int32 {
		return outer(tls, cb.pOuterCtx, tflags, pToken+uintptr(off), int32(n), start, end)
	})
}

// Creates a tokenizer instance wrapping a unicode61 instance built from the same arguments
func cjkCreate(tls *libc.TLS, pApiCtx, azArg uintptr, nArg int32, ppOut uintptr) int32 {
	p := sqlite3.Xsqlite3_malloc(tls, int32(unsafe.Sizeof(cjkTokenizer{})))
	if p == 0 {
		return sqlite3.SQLITE_NOMEM
	}
	tok := (*cjkTokenizer)(cMem(p))
	*tok = cjkTokenizer{}
	api := (*sqlite3.Tfts5_api)(cMem(pApiCtx))
	slot := tls.Alloc(int(unsafe.Sizeof(uintptr(0))))
	defer tls.Free(int(unsafe.Sizeof(uintptr(0))))
	*(*uintptr)(cMem(slot)) = 0
	name := tlsCString(tls, "unicode61")
	rc := cFunc[fts5FindTokenizer](api.FxFindTokenizer)(tls, pApiCtx, name, slot, p)
	tls.Free(len("unicode61") + 1)
	if rc == sqlite3.SQLITE_OK {
		rc = cFunc[fts5CreateFn](tok.inner.FxCreate)(tls, *(*uintptr)(cMem(slot)), azArg, nArg, p+unsafe.Offsetof(tok.pInner))
	}
	if rc != sqlite3.SQLITE_OK {
		sqlite3.Xsqlite3_free(tls, p)
		return rc
	}
	*(*uintptr)(cMem(ppOut)) = p
	return sqlite3.SQLITE_OK
}

// Deletes the wrapped unicode61 instance and frees the tokenizer
func cjkDelete(tls *libc.TLS, pTok uintptr) {
	if pTok == 0 {
		return
	}
	tok := (*cjkTokenizer)(cMem(pTok))
	if tok.pInner != 0 {
		cFunc[fts5DeleteFn](tok.inner.FxDelete)(tls, tok.pInner)
	}
	sqlite3.Xsqlite3_free(tls, pTok)
}

// Tokenizes through unicode61 with every emitted token re-split by the CJK bigram rule
func cjkTokenize(tls *libc.TLS, pTok, pCtx uintptr, flags int32, pText uintptr, nText int32, xToken uintptr) int32 {
	tok := (*cjkTokenizer)(cMem(pTok))
	size := int(unsafe.Sizeof(cjkCallbackCtx{}))
	cb := tls.Alloc(size)
	defer tls.Free(size)
	*(*cjkCallbackCtx)(cMem(cb)) = cjkCallbackCtx{pOuterCtx: pCtx, xOuterToken: xToken}
	return cFunc[fts5TokenizeFn](tok.inner.FxTokenize)(tls, tok.pInner, cb, flags, pText, nText, cFuncPtr(cjkInnerCallback))
}

// fts5_api of a connection through SELECT fts5(?1), zero when FTS5 is unavailable
func cjkFTS5API(tls *libc.TLS, db uintptr) uintptr {
	word := int(unsafe.Sizeof(uintptr(0)))
	slots := tls.Alloc(2 * word)
	defer tls.Free(2 * word)
	pRet, ppStmt := slots, slots+uintptr(word)
	*(*uintptr)(cMem(pRet)), *(*uintptr)(cMem(ppStmt)) = 0, 0
	query := tlsCString(tls, "SELECT fts5(?1)")
	defer tls.Free(len("SELECT fts5(?1)") + 1)
	ptrType := tlsCString(tls, "fts5_api_ptr")
	defer tls.Free(len("fts5_api_ptr") + 1)
	if sqlite3.Xsqlite3_prepare_v2(tls, db, query, -1, ppStmt, 0) == sqlite3.SQLITE_OK {
		stmt := *(*uintptr)(cMem(ppStmt))
		sqlite3.Xsqlite3_bind_pointer(tls, stmt, 1, pRet, ptrType, 0)
		sqlite3.Xsqlite3_step(tls, stmt)
	}
	sqlite3.Xsqlite3_finalize(tls, *(*uintptr)(cMem(ppStmt)))
	return *(*uintptr)(cMem(pRet))
}

// Registers cjk_unicode61 on one connection as sqlite3_ftscjk_init does
func registerCJKTokenizer(tls *libc.TLS, db uintptr) error {
	api := cjkFTS5API(tls, db)
	if api == 0 {
		return errors.New("fts5_cjk: FTS5 unavailable")
	}
	size := int(unsafe.Sizeof(sqlite3.Tfts5_tokenizer{}))
	tok := tls.Alloc(size)
	defer tls.Free(size)
	*(*sqlite3.Tfts5_tokenizer)(cMem(tok)) = sqlite3.Tfts5_tokenizer{
		FxCreate:   cFuncPtr(cjkCreate),
		FxDelete:   cFuncPtr(cjkDelete),
		FxTokenize: cFuncPtr(cjkTokenize),
	}
	name := tlsCString(tls, "cjk_unicode61")
	defer tls.Free(len("cjk_unicode61") + 1)
	rc := cFunc[fts5CreateTokenize]((*sqlite3.Tfts5_api)(cMem(api)).FxCreateTokenizer)(tls, api, name, api, tok, 0)
	if rc != sqlite3.SQLITE_OK {
		return &SQLiteError{Class: classForCode(int(rc)), Msg: sqliteErrstr(int(rc)), Code: int(rc), HasCode: true}
	}
	return nil
}
