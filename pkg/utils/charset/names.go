package charsetutil

import (
	"strings"
	"sync"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// Character set of a label: this package's names and aliases first, then the Windows names of the system code pages, then the IANA and WHATWG label indexes of x/text
func find(name string) (*charset, bool) {
	var buf [48]byte
	sets := registry()
	key := labelKey(buf[:0], name)
	if cs, ok := sets[string(key)]; ok {
		return cs, true
	}
	if label, ok := platformLabel(string(key)); ok {
		cs, ok := sets[string(labelKey(buf[:0], label))]
		return cs, ok
	}
	if enc, err := ianaindex.IANA.Encoding(name); err == nil && enc != nil {
		if canonical, err := ianaindex.MIME.Name(enc); err == nil {
			if cs, ok := sets[string(labelKey(buf[:0], canonical))]; ok {
				return cs, true
			}
		}
	}
	if enc, err := htmlindex.Get(name); err == nil {
		if canonical, err := htmlindex.Name(enc); err == nil {
			if cs, ok := sets[string(labelKey(buf[:0], canonical))]; ok {
				return cs, true
			}
		}
	}
	return nil, false
}

// Loose form of a label that matching compares: ASCII letters lower cased and digits kept, everything else dropped, and a zero dropped where it leads a number, so UTF-8, utf_8 and utf8 or cp037 and CP-37 meet
func labelKey(dst []byte, label string) []byte {
	afterDigit := false
	for i := 0; i < len(label); i++ {
		switch c := label[i]; {
		case c >= 'a' && c <= 'z':
			dst, afterDigit = append(dst, c), false
		case c >= 'A' && c <= 'Z':
			dst, afterDigit = append(dst, c+'a'-'A'), false
		case c == '0' && !afterDigit && i+1 < len(label) && label[i+1] >= '0' && label[i+1] <= '9':
		case c >= '0' && c <= '9':
			dst, afterDigit = append(dst, c), true
		default:
			afterDigit = false
		}
	}
	return dst
}

// Bytes GB18030 gives U+FFFD itself, the one sequence whose replacement character is not a decoding failure
const gb18030Replacement = "\x84\x31\xa4\x37"

// Every character set by the loose key of its canonical name and of each alias, built on first use
var registry = sync.OnceValue(func() map[string]*charset {
	sets := make(map[string]*charset, 1100)
	add := func(name string, c codec, aliases string) {
		cs := &charset{name: name, codec: c}
		put := func(label string) {
			key := string(labelKey(nil, label))
			if _, taken := sets[key]; !taken {
				sets[key] = cs
			}
		}
		put(name)
		for label := range strings.FieldsSeq(aliases) {
			put(label)
		}
	}

	add("UTF-8", utf8Codec{}, "utf8 u8 utf unicode-1-1-utf-8 unicode-2-0-utf-8 unicode20utf8 x-unicode20utf8 csutf8 cp65001 utf8-ucs2 utf8-ucs4")
	add("UTF-8-BOM", utf8BOMCodec{}, "utf-8-sig utf8-sig utf8bom")
	add("UTF-16", utf16Codec{bom: true}, "utf16 u16 csutf16 ucs-2 csunicode iso-10646-ucs-2 unicode")
	add("UTF-16LE", utf16Codec{}, "utf-16-le csutf16le unicodefeff unicodelittleunmarked cp1200 ucs-2le")
	add("UTF-16BE", utf16Codec{big: true}, "utf-16-be csutf16be unicodefffe unicodebigunmarked cp1201 ucs-2be")
	add("UTF-32", utf32Codec{bom: true}, "utf32 u32 csutf32 ucs-4 csucs4 iso-10646-ucs-4")
	add("UTF-32LE", utf32Codec{}, "utf-32-le csutf32le cp12000 ucs-4le")
	add("UTF-32BE", utf32Codec{big: true}, "utf-32-be csutf32be cp12001 ucs-4be")
	add("UTF-7", utf7Codec{}, "utf7 u7 csutf7 unicode-1-1-utf-7 csunicode11utf7 cp65000")

	add("US-ASCII", asciiCodec{}, "ascii us 646 iso646-us iso_646.irv:1991 ansi_x3.4-1968 ansi_x3.4-1986 iso-ir-6 ibm367 cp367 csascii cp20127")
	add("ISO-8859-1", singleCodec{charmap.ISO8859_1}, "iso_8859-1:1987 iso-ir-100 latin1 l1 csisolatin1 ibm819 cp819 8859 iso8859 latin cp28591")
	add("ISO-8859-2", singleCodec{withC1{charmap.ISO8859_2}}, "iso_8859-2:1987 iso-ir-101 latin2 l2 csisolatin2 cp28592")
	add("ISO-8859-3", singleCodec{withC1{charmap.ISO8859_3}}, "iso_8859-3:1988 iso-ir-109 latin3 l3 csisolatin3 cp28593")
	add("ISO-8859-4", singleCodec{withC1{charmap.ISO8859_4}}, "iso_8859-4:1988 iso-ir-110 latin4 l4 csisolatin4 cp28594")
	add("ISO-8859-5", singleCodec{withC1{charmap.ISO8859_5}}, "iso_8859-5:1988 iso-ir-144 cyrillic csisolatincyrillic cp28595")
	add("ISO-8859-6", singleCodec{withC1{charmap.ISO8859_6}}, "iso_8859-6:1987 iso-ir-127 arabic asmo-708 ecma-114 csisolatinarabic iso-8859-6-e iso-8859-6-i csiso88596e csiso88596i cp28596 cp708")
	add("ISO-8859-7", singleCodec{withC1{charmap.ISO8859_7}}, "iso_8859-7:1987 iso-ir-126 greek greek8 ecma-118 elot_928 csisolatingreek sun_eu_greek cp28597")
	add("ISO-8859-8", singleCodec{withC1{charmap.ISO8859_8}}, "iso_8859-8:1988 iso-ir-138 hebrew visual csisolatinhebrew iso-8859-8-e csiso88598e cp28598")
	add("ISO-8859-8-I", singleCodec{withC1{charmap.ISO8859_8}}, "logical csiso88598i cp38598")
	add("ISO-8859-9", singleCodec{charmap.ISO8859_9}, "iso_8859-9:1989 iso-ir-148 latin5 l5 csisolatin5 cp28599")
	add("ISO-8859-10", singleCodec{withC1{charmap.ISO8859_10}}, "iso_8859-10:1992 iso-ir-157 latin6 l6 csisolatin6")
	add("ISO-8859-13", singleCodec{withC1{charmap.ISO8859_13}}, "latin7 l7 csiso885913 cp28603")
	add("ISO-8859-14", singleCodec{withC1{charmap.ISO8859_14}}, "iso_8859-14:1998 iso-ir-199 latin8 l8 iso-celtic csiso885914")
	add("ISO-8859-15", singleCodec{withC1{charmap.ISO8859_15}}, "latin9 l9 csisolatin9 csiso885915 cp28605")
	add("ISO-8859-16", singleCodec{withC1{charmap.ISO8859_16}}, "iso_8859-16:2001 iso-ir-226 latin10 l10 csiso885916")
	add("windows-874", singleCodec{charmap.Windows874}, "cp874 ms874 874 dos-874")
	add("TIS-620", singleCodec{withC1{charmap.Windows874}}, "iso-8859-11 iso_8859-11:2001 tis_620-0 tis_620-2529-0 tis_620-2529-1 iso-ir-166 thai cstis620")
	add("windows-1250", singleCodec{charmap.Windows1250}, "cp1250 x-cp1250 1250")
	add("windows-1251", singleCodec{charmap.Windows1251}, "cp1251 x-cp1251 1251")
	add("windows-1252", singleCodec{charmap.Windows1252}, "cp1252 x-cp1252 1252")
	add("windows-1253", singleCodec{charmap.Windows1253}, "cp1253 x-cp1253 1253")
	add("windows-1254", singleCodec{charmap.Windows1254}, "cp1254 x-cp1254 1254")
	add("windows-1255", singleCodec{charmap.Windows1255}, "cp1255 x-cp1255 1255")
	add("windows-1256", singleCodec{charmap.Windows1256}, "cp1256 x-cp1256 1256")
	add("windows-1257", singleCodec{charmap.Windows1257}, "cp1257 x-cp1257 1257")
	add("windows-1258", singleCodec{charmap.Windows1258}, "cp1258 x-cp1258 1258")
	add("KOI8-R", singleCodec{charmap.KOI8R}, "koi koi8 cskoi8r cp20866")
	add("KOI8-U", singleCodec{charmap.KOI8U}, "koi8-ru cskoi8u cp21866")
	add("KOI8-T", singleCodec{&byteTable{chars: koi8T}}, "")
	add("macintosh", singleCodec{charmap.Macintosh}, "mac x-mac-roman csmacintosh macroman cp10000")
	add("x-mac-cyrillic", singleCodec{charmap.MacintoshCyrillic}, "x-mac-ukrainian mac-cyrillic maccyrillic cp10007 cp10017")
	add("x-mac-ce", singleCodec{&byteTable{chars: macCentralEurope}}, "x-mac-centraleurroman mac-latin2 maclatin2 maccentraleurope mac-centeuro cp10029")
	add("x-mac-croatian", singleCodec{&byteTable{chars: macCroatian}}, "mac-croatian maccroatian cp10082")
	add("x-mac-greek", singleCodec{&byteTable{chars: macGreek}}, "mac-greek macgreek cp10006")
	add("x-mac-icelandic", singleCodec{&byteTable{chars: macIcelandic}}, "mac-iceland maciceland macicelandic cp10079")
	add("x-mac-romanian", singleCodec{&byteTable{chars: macRomanian}}, "mac-romanian macromanian cp10010")
	add("x-mac-turkish", singleCodec{&byteTable{chars: macTurkish}}, "mac-turkish macturkish cp10081")
	add("x-mac-arabic", singleCodec{&byteTable{chars: macArabic}}, "mac-arabic macarabic cp10004")
	add("x-mac-farsi", singleCodec{&byteTable{chars: macFarsi}}, "mac-farsi macfarsi")
	add("x-user-defined", singleCodec{charmap.XUserDefined}, "")

	add("IBM037", singleCodec{charmap.CodePage037}, "cp037 037 ibm039 csibm037 ebcdic-cp-us ebcdic-cp-ca ebcdic-cp-wt ebcdic-cp-nl")
	add("IBM273", singleCodec{&byteTable{chars: ibm273}}, "cp273 273 csibm273 cp20273")
	add("IBM424", singleCodec{&byteTable{chars: ibm424}}, "cp424 424 ebcdic-cp-he csibm424 cp20424")
	add("IBM437", singleCodec{charmap.CodePage437}, "cp437 437 cspc8codepage437")
	add("IBM500", singleCodec{&byteTable{chars: ibm500}}, "cp500 500 ebcdic-cp-be ebcdic-cp-ch csibm500")
	add("DOS-720", singleCodec{&byteTable{chars: dos720}}, "cp720 720")
	add("IBM737", singleCodec{&byteTable{chars: ibm737}}, "cp737 737")
	add("IBM775", singleCodec{&byteTable{chars: ibm775}}, "cp775 775 cspc775baltic")
	add("IBM850", singleCodec{charmap.CodePage850}, "cp850 850 cspc850multilingual")
	add("IBM852", singleCodec{charmap.CodePage852}, "cp852 852 cspcp852")
	add("IBM855", singleCodec{charmap.CodePage855}, "cp855 855 csibm855")
	add("IBM856", singleCodec{&byteTable{chars: ibm856}}, "cp856")
	add("IBM857", singleCodec{&byteTable{chars: ibm857}}, "cp857 857 csibm857")
	add("IBM00858", singleCodec{charmap.CodePage858}, "cp858 858 ccsid00858 cp00858 pc-multilingual-850+euro csibm00858")
	add("IBM860", singleCodec{charmap.CodePage860}, "cp860 860 csibm860")
	add("IBM861", singleCodec{&byteTable{chars: ibm861}}, "cp861 861 cp-is csibm861")
	add("IBM862", singleCodec{charmap.CodePage862}, "cp862 862 cspc862latinhebrew")
	add("IBM863", singleCodec{charmap.CodePage863}, "cp863 863 csibm863")
	add("IBM864", singleCodec{&byteTable{chars: ibm864}}, "cp864 864 csibm864")
	add("IBM865", singleCodec{charmap.CodePage865}, "cp865 865 csibm865")
	add("IBM866", singleCodec{charmap.CodePage866}, "cp866 866 csibm866")
	add("IBM869", singleCodec{&byteTable{chars: ibm869}}, "cp869 869 cp-gr csibm869")
	add("IBM875", singleCodec{&byteTable{chars: ibm875}}, "cp875")
	add("IBM1006", singleCodec{&byteTable{chars: ibm1006}}, "cp1006")
	add("IBM1026", singleCodec{&byteTable{chars: ibm1026}}, "cp1026 1026 csibm1026")
	add("IBM1047", singleCodec{charmap.CodePage1047}, "cp1047 1047 csibm1047")
	add("IBM1125", singleCodec{&byteTable{chars: ibm1125}}, "cp1125 1125 cp866u ruscii")
	add("IBM01140", singleCodec{charmap.CodePage1140}, "cp1140 1140 ccsid01140 cp01140 ebcdic-us-37+euro csibm01140")
	add("hp-roman8", singleCodec{&byteTable{chars: hpRoman8}}, "roman8 r8 cshproman8 cp1051 ibm1051")
	add("KZ-1048", singleCodec{&byteTable{chars: kz1048}}, "strk1048-2002 rk1048 cskz1048")
	add("PTCP154", singleCodec{&byteTable{chars: ptcp154}}, "pt154 cp154 csptcp154 cyrillic-asian")
	add("palmos", singleCodec{&byteTable{chars: palmOS}}, "")

	add("Shift_JIS", xcodec{enc: japanese.ShiftJIS}, "shift-jis sjis s-jis x-sjis ms_kanji mskanji csshiftjis windows-31j cswindows31j ms932 cp932 932 x-mac-japanese cp10001")
	add("EUC-JP", xcodec{enc: japanese.EUCJP}, "eucjp x-euc-jp cseucpkdfmtjapanese extended_unix_code_packed_format_for_japanese ujis u-jis cp51932 cp20932")
	add("ISO-2022-JP", xcodec{enc: japanese.ISO2022JP}, "csiso2022jp iso-2022-jp-ext cp50220 cp50221")
	add("ISO-2022-JP-2", iso2022JP2{}, "csiso2022jp2 iso-2022-jp-1")
	add("EUC-KR", xcodec{enc: korean.EUCKR}, "euckr cseuckr csksc56011987 iso-ir-149 korean ks_c_5601-1987 ks_c_5601-1989 ksc5601 ks_x_1001 windows-949 cp949 ms949 949 uhc x-mac-korean cp10003 x-cp20949 cp20949 cp51949")
	add("ISO-2022-KR", iso2022KR{}, "csiso2022kr cp50225")
	add("GBK", xcodec{enc: simplifiedchinese.GBK}, "cp936 ms936 936 windows-936 x-gbk csgbk gb2312 csgb2312 csiso58gb231280 gb_2312-80 gb2312-1980 iso-ir-58 chinese euc-cn eucgb2312-cn x-mac-simp-chinese x-mac-chinesesimp cp10008 x-cp20936 cp20936 cp51936")
	add("GB18030", xcodec{enc: simplifiedchinese.GB18030, fffd: gb18030Replacement}, "gb18030-2000 csgb18030 windows-54936 cp54936")
	add("HZ-GB-2312", xcodec{enc: simplifiedchinese.HZGB2312}, "hz hz-gb cp52936")
	add("Big5", xcodec{enc: traditionalchinese.Big5}, "big5-hkscs hkscs csbig5hkscs cn-big5 csbig5 x-x-big5 big5-tw cp950 ms950 950 windows-950 x-mac-trad-chinese x-mac-chinesetrad cp10002")
	return sets
})
