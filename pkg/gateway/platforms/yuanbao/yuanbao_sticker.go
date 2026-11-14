package yuanbao

import (
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"golang.org/x/text/unicode/norm"
)

var stickerRows = [][3]string{
	{"六六六", "278", "666 厉害 牛 棒 绝了 好强 awesome"},
	{"我想开了", "262", "想开 佛系 释怀 顿悟 看淡了 无所谓"},
	{"害羞", "130", "腼腆 不好意思 脸红 娇羞 羞涩 捂脸"},
	{"比心", "252", "笔芯 爱你 爱心手势 love heart 喜欢你"},
	{"委屈", "125", "难过 想哭 可怜巴巴 瘪嘴 受伤 被欺负"},
	{"亲亲", "146", "么么 mua 亲一下 kiss 飞吻 啵"},
	{"酷", "131", "帅 墨镜 cool 高冷 有型 swagger"},
	{"睡", "145", "睡觉 困 zzZ 打盹 躺平 休眠 sleepy"},
	{"发呆", "152", "懵 愣住 放空 呆滞 出神 脑子空白"},
	{"可怜", "157", "卖萌 求饶 委屈巴巴 弱小 拜托 眼巴巴"},
	{"摊手", "200", "无奈 没办法 耸肩 随便 那咋整 whatever"},
	{"头大", "213", "头疼 烦恼 郁闷 难搞 崩溃 一团乱"},
	{"吓", "256", "害怕 惊恐 震惊 吓一跳 恐怖 怂"},
	{"吐血", "203", "无语 崩溃 被雷 内伤 一口老血 屮"},
	{"哼", "185", "傲娇 生气 不满 撇嘴 不理 赌气"},
	{"嘿嘿", "220", "坏笑 猥琐笑 偷笑 憨笑 得意 你懂的"},
	{"头秃", "218", "程序员 加班 焦虑 没头发 秃了 肝爆"},
	{"暗中观察", "221", "窥屏 潜水 偷偷看 角落 围观 屏住呼吸"},
	{"我酸了", "224", "嫉妒 柠檬精 羡慕 吃柠檬 眼红 恰柠檬"},
	{"打call", "246", "应援 加油 支持 喝彩 助威 call"},
	{"庆祝", "251", "祝贺 开心 耶 party 胜利 干杯"},
	{"奋斗", "151", "努力 加油 拼搏 冲 干劲 卷起来"},
	{"惊讶", "143", "震惊 哇 不敢相信 OMG 居然 这么离谱"},
	{"疑问", "144", "问号 不懂 啥 为什么 啥情况 懵逼问"},
	{"仔细分析", "248", "思考 推敲 认真 研究 琢磨 让我想想"},
	{"撅嘴", "184", "嘟嘴 卖萌 不高兴 撒娇 嘴翘"},
	{"泪奔", "199", "大哭 伤心 破防 感动哭 泪流满面 呜呜"},
	{"尊嘟假嘟", "276", "真的假的 真假 可爱问 你骗我 是不是"},
	{"略略略", "113", "调皮 吐舌 不服 略 气死你 鬼脸"},
	{"困", "180", "想睡 倦 打哈欠 睁不开眼 好困啊 sleepy"},
	{"折磨", "181", "难受 痛苦 煎熬 蚌埠住了 受不了 要命"},
	{"抠鼻", "182", "不屑 无聊 淡定 无所谓 鄙视 挖鼻"},
	{"鼓掌", "183", "拍手 叫好 赞同 666 喝彩 掌声"},
	{"斜眼笑", "204", "滑稽 坏笑 doge 意味深长 阴阳怪气 嘿嘿嘿"},
	{"辣眼睛", "216", "看不下去 cringe 毁三观 太丑了 瞎了"},
	{"哦哟", "217", "惊讶 起哄 哇哦 有戏 不简单 哟"},
	{"吃瓜", "222", "围观 看戏 八卦 路人 看热闹 板凳"},
	{"狗头", "225", "doge 保命 开玩笑 滑稽 反讽 懂的都懂"},
	{"敬礼", "227", "salute 尊重 收到 遵命 致敬 报告"},
	{"哦", "231", "知道了 明白 敷衍 嗯 这样啊 收到"},
	{"拿到红包", "236", "红包 谢谢老板 发财 开心 抢到了 欧气"},
	{"牛吖", "239", "牛 厉害 强 666 佩服 大佬"},
	{"贴贴", "272", "抱抱 亲昵 蹭蹭 亲密 靠靠 撒娇贴"},
	{"爱心", "138", "心 love 喜欢你 红心 示爱 么么哒"},
	{"晚安", "170", "好梦 睡了 night 早点休息 安啦 moon"},
	{"太阳", "176", "晴天 早上好 阳光 morning 好天气 日"},
	{"柠檬", "266", "酸 嫉妒 柠檬精 羡慕 我酸 恰柠檬"},
	{"大冤种", "267", "倒霉 吃亏 自嘲 好心没好报 背锅 工具人"},
	{"吐了", "132", "恶心 yue 受不了 嫌弃 想吐 生理不适"},
	{"怒", "134", "生气 愤怒 火大 暴躁 气炸 怼"},
	{"玫瑰", "165", "花 示爱 表白 浪漫 送你花 情人节"},
	{"凋谢", "119", "花谢 失恋 难过 枯萎 心碎 凉了"},
	{"点赞", "159", "赞 认同 好棒 good like 大拇指 顶"},
	{"握手", "164", "合作 你好 商务 hello deal 成交 友好"},
	{"抱拳", "163", "谢谢 失敬 江湖 承让 拜托 有礼"},
	{"ok", "169", "好的 收到 没问题 okay 行 可以 懂了"},
	{"拳头", "174", "加油 干 冲 fight 力量 击拳 硬气"},
	{"鞭炮", "191", "过年 喜庆 爆竹 春节 噼里啪啦 红"},
	{"烟花", "258", "庆典 漂亮 新年 嘭 绽放 节日快乐"},
}

// Builtin catalogue keyed by name, each value a sticker dict in package 1003 at 128x128 png
var StickerMap = buildStickerMap()

func buildStickerMap() *pyjson.Object {
	m := pyjson.NewObject()
	for _, row := range stickerRows {
		s := pyjson.NewObject()
		s.Set("sticker_id", row[1])
		s.Set("package_id", "1003")
		s.Set("name", row[0])
		s.Set("description", row[2])
		s.Set("width", 128)
		s.Set("height", 128)
		s.Set("formats", "png")
		m.Set(row[0], s)
	}
	return m
}

// Missing key error a sticker dict lookup raises, rendered as str of the Python KeyError with quotes
type KeyError struct{ Key string }

func (e *KeyError) Error() string { return pyrepr.StrRepr(e.Key) }

func stickers() []*pyjson.Object {
	out := make([]*pyjson.Object, 0, StickerMap.Len())
	for _, k := range StickerMap.Keys {
		out = append(out, StickerMap.Vals[k].(*pyjson.Object))
	}
	return out
}

func stickerField(s *pyjson.Object, key string) string {
	v, _ := s.Get(key)
	text, _ := v.(string)
	return text
}

// Exact name, then mutual substring of name, then description containment, then the best fuzzy hit, nil when none
func GetStickerByName(name string) *pyjson.Object {
	if name == "" {
		return nil
	}
	query := pystr.Strip(name)
	if v, ok := StickerMap.Get(query); ok {
		return v.(*pyjson.Object)
	}
	for _, key := range StickerMap.Keys {
		if strings.Contains(key, query) || strings.Contains(query, key) {
			return StickerMap.Vals[key].(*pyjson.Object)
		}
	}
	for _, s := range stickers() {
		if strings.Contains(stickerField(s, "description"), query) {
			return s
		}
	}
	if matches := SearchStickers(query, 1); len(matches) > 0 {
		return matches[0]
	}
	return nil
}

// Random sticker, drawn from those whose description or name contains category when any do, else from all
func GetRandomSticker(category string) *pyjson.Object {
	var candidates []*pyjson.Object
	if category != "" {
		for _, s := range stickers() {
			if strings.Contains(stickerField(s, "description"), category) || strings.Contains(stickerField(s, "name"), category) {
				candidates = append(candidates, s)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = stickers()
	}
	return candidates[rand.IntN(len(candidates))]
}

// Sticker whose id equals the stripped id, nil when the id is empty or unknown
func GetStickerByID(stickerID string) *pyjson.Object {
	if stickerID == "" {
		return nil
	}
	sid := pystr.Strip(stickerID)
	for _, s := range stickers() {
		if stickerField(s, "sticker_id") == sid {
			return s
		}
	}
	return nil
}

var punctRe = func() *regexp2.Regexp {
	re, err := pyre.Compile(`[\s\u3000\-_·.,，。!！?？\"“”'‘’、/\\]+`)
	if err != nil {
		panic(err)
	}
	return re
}()

func normalizeText(raw string) string {
	return pystr.Lower(pystr.Strip(norm.NFKC.String(raw)))
}

func compactText(raw string) string {
	out, err := punctRe.Replace(normalizeText(raw), "", -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

func multisetCharHitRatio(needle, haystack []rune) float64 {
	if len(needle) == 0 {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range haystack {
		counts[r]++
	}
	hits := 0
	for _, r := range needle {
		if counts[r] > 0 {
			counts[r]--
			hits++
		}
	}
	return float64(hits) / float64(len(needle))
}

func bigrams(x []rune) map[string]struct{} {
	set := map[string]struct{}{}
	for i := 0; i < len(x)-1; i++ {
		set[string(x[i:i+2])] = struct{}{}
	}
	return set
}

func bigramJaccard(a, b []rune) float64 {
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	setA, setB := bigrams(a), bigrams(b)
	inter := 0
	for g := range setA {
		if _, ok := setB[g]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(setA)+len(setB)-inter)
}

func longestSubsequenceRatio(needle, haystack []rune) float64 {
	if len(needle) == 0 {
		return 0
	}
	j := 0
	for _, ch := range haystack {
		if j >= len(needle) {
			break
		}
		if ch == needle[j] {
			j++
		}
	}
	return float64(j) / float64(len(needle))
}

func scoreField(haystack, query string) float64 {
	hay, q := normalizeText(haystack), normalizeText(query)
	if hay == "" || q == "" {
		return 0
	}
	hayC, qC := []rune(compactText(haystack)), []rune(compactText(query))
	qLen := len([]rune(q))
	best := 0.0
	consider := func(ok bool, v float64) {
		if ok && v > best {
			best = v
		}
	}
	consider(hay == q, 100)
	consider(strings.Contains(hay, q), float64(92+min(6, qLen)))
	consider(qLen >= 2 && strings.HasPrefix(hay, q), 88)
	consider(len(qC) > 0 && strings.Contains(string(hayC), string(qC)), 86)
	consider(true, multisetCharHitRatio(qC, hayC)*62)
	consider(true, bigramJaccard(qC, hayC)*58)
	consider(true, longestSubsequenceRatio(qC, hayC)*52)
	consider(qLen == 1 && strings.Contains(hay, q), 68)
	return best
}

// Builtin stickers ranked by fuzzy match on name, description and id, table order for an empty query
func SearchStickers(query string, limit int) []*pyjson.Object {
	if limit == 0 {
		limit = 10
	}
	safeLimit := max(1, min(500, limit))
	all := stickers()
	qNorm := normalizeText(query)
	if query == "" || qNorm == "" {
		return all[:min(safeLimit, len(all))]
	}
	type scoredSticker struct {
		score   float64
		sticker *pyjson.Object
	}
	scored := make([]scoredSticker, 0, len(all))
	for _, s := range all {
		sidNorm := normalizeText(pystr.Strip(stickerField(s, "sticker_id")))
		idScore := 0.0
		if sidNorm == qNorm {
			idScore = 100
		} else if sidNorm != "" && strings.Contains(sidNorm, qNorm) {
			idScore = 84
		}
		score := max(scoreField(stickerField(s, "name"), query), scoreField(stickerField(s, "description"), query)*0.88, idScore)
		scored = append(scored, scoredSticker{score, s})
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	pick := func(pairs []scoredSticker) []*pyjson.Object {
		out := make([]*pyjson.Object, 0, safeLimit)
		for _, p := range pairs[:min(safeLimit, len(pairs))] {
			out = append(out, p.sticker)
		}
		return out
	}
	top := scored[0].score
	if top <= 0 {
		return pick(scored)
	}
	var floor float64
	switch {
	case top >= 22:
		floor = 18
	case top >= 12:
		floor = max(10, top*0.5)
	default:
		floor = max(6, top*0.35)
	}
	var filtered []scoredSticker
	for _, p := range scored {
		if p.score >= floor {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		filtered = scored
	}
	return pick(filtered)
}

// TIMFaceElem message body carrying faceIndex as index and the sticker data string only when one is given
func BuildFaceMsgBody(faceIndex int, data *string) []any {
	content := pyjson.NewObject()
	content.Set("index", faceIndex)
	if data != nil {
		content.Set("data", *data)
	}
	elem := pyjson.NewObject()
	elem.Set("msg_type", "TIMFaceElem")
	elem.Set("msg_content", content)
	return []any{elem}
}

// TIMFaceElem message body at index 0 whose data is the compact JSON metadata of the sticker
func BuildStickerMsgBody(sticker *pyjson.Object) ([]any, error) {
	get := func(key string, def any) (any, error) {
		if v, ok := sticker.Get(key); ok {
			return v, nil
		}
		if def == nil {
			return nil, &KeyError{Key: key}
		}
		return def, nil
	}
	payload := pyjson.NewObject()
	for _, f := range []struct {
		key string
		def any
	}{{"sticker_id", nil}, {"package_id", nil}, {"width", 128}, {"height", 128}, {"formats", "png"}, {"name", nil}} {
		v, err := get(f.key, f.def)
		if err != nil {
			return nil, err
		}
		payload.Set(f.key, v)
	}
	data, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	return BuildFaceMsgBody(0, &data), nil
}
