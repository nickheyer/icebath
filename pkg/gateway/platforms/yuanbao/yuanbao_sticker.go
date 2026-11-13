package yuanbao

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Builtin TIM face sticker of package 1003
type sticker struct{ ID, Name, Description string }

const stickerPackageID = "1003"

var stickers = []sticker{
	{"278", "六六六", "666 厉害 牛 棒 绝了 好强 awesome"},
	{"262", "我想开了", "想开 佛系 释怀 顿悟 看淡了 无所谓"},
	{"130", "害羞", "腼腆 不好意思 脸红 娇羞 羞涩 捂脸"},
	{"252", "比心", "笔芯 爱你 爱心手势 love heart 喜欢你"},
	{"125", "委屈", "难过 想哭 可怜巴巴 瘪嘴 受伤 被欺负"},
	{"146", "亲亲", "么么 mua 亲一下 kiss 飞吻 啵"},
	{"131", "酷", "帅 墨镜 cool 高冷 有型 swagger"},
	{"145", "睡", "睡觉 困 zzZ 打盹 躺平 休眠 sleepy"},
	{"152", "发呆", "懵 愣住 放空 呆滞 出神 脑子空白"},
	{"157", "可怜", "卖萌 求饶 委屈巴巴 弱小 拜托 眼巴巴"},
	{"200", "摊手", "无奈 没办法 耸肩 随便 那咋整 whatever"},
	{"213", "头大", "头疼 烦恼 郁闷 难搞 崩溃 一团乱"},
	{"256", "吓", "害怕 惊恐 震惊 吓一跳 恐怖 怂"},
	{"203", "吐血", "无语 崩溃 被雷 内伤 一口老血 屮"},
	{"185", "哼", "傲娇 生气 不满 撇嘴 不理 赌气"},
	{"220", "嘿嘿", "坏笑 猥琐笑 偷笑 憨笑 得意 你懂的"},
	{"218", "头秃", "程序员 加班 焦虑 没头发 秃了 肝爆"},
	{"221", "暗中观察", "窥屏 潜水 偷偷看 角落 围观 屏住呼吸"},
	{"224", "我酸了", "嫉妒 柠檬精 羡慕 吃柠檬 眼红 恰柠檬"},
	{"246", "打call", "应援 加油 支持 喝彩 助威 call"},
	{"251", "庆祝", "祝贺 开心 耶 party 胜利 干杯"},
	{"151", "奋斗", "努力 加油 拼搏 冲 干劲 卷起来"},
	{"143", "惊讶", "震惊 哇 不敢相信 OMG 居然 这么离谱"},
	{"144", "疑问", "问号 不懂 啥 为什么 啥情况 懵逼问"},
	{"248", "仔细分析", "思考 推敲 认真 研究 琢磨 让我想想"},
	{"184", "撅嘴", "嘟嘴 卖萌 不高兴 撒娇 嘴翘"},
	{"199", "泪奔", "大哭 伤心 破防 感动哭 泪流满面 呜呜"},
	{"276", "尊嘟假嘟", "真的假的 真假 可爱问 你骗我 是不是"},
	{"113", "略略略", "调皮 吐舌 不服 略 气死你 鬼脸"},
	{"180", "困", "想睡 倦 打哈欠 睁不开眼 好困啊 sleepy"},
	{"181", "折磨", "难受 痛苦 煎熬 蚌埠住了 受不了 要命"},
	{"182", "抠鼻", "不屑 无聊 淡定 无所谓 鄙视 挖鼻"},
	{"183", "鼓掌", "拍手 叫好 赞同 666 喝彩 掌声"},
	{"204", "斜眼笑", "滑稽 坏笑 doge 意味深长 阴阳怪气 嘿嘿嘿"},
	{"216", "辣眼睛", "看不下去 cringe 毁三观 太丑了 瞎了"},
	{"217", "哦哟", "惊讶 起哄 哇哦 有戏 不简单 哟"},
	{"222", "吃瓜", "围观 看戏 八卦 路人 看热闹 板凳"},
	{"225", "狗头", "doge 保命 开玩笑 滑稽 反讽 懂的都懂"},
	{"227", "敬礼", "salute 尊重 收到 遵命 致敬 报告"},
	{"231", "哦", "知道了 明白 敷衍 嗯 这样啊 收到"},
	{"236", "拿到红包", "红包 谢谢老板 发财 开心 抢到了 欧气"},
	{"239", "牛吖", "牛 厉害 强 666 佩服 大佬"},
	{"272", "贴贴", "抱抱 亲昵 蹭蹭 亲密 靠靠 撒娇贴"},
	{"138", "爱心", "心 love 喜欢你 红心 示爱 么么哒"},
	{"170", "晚安", "好梦 睡了 night 早点休息 安啦 moon"},
	{"176", "太阳", "晴天 早上好 阳光 morning 好天气 日"},
	{"266", "柠檬", "酸 嫉妒 柠檬精 羡慕 我酸 恰柠檬"},
	{"267", "大冤种", "倒霉 吃亏 自嘲 好心没好报 背锅 工具人"},
	{"132", "吐了", "恶心 yue 受不了 嫌弃 想吐 生理不适"},
	{"134", "怒", "生气 愤怒 火大 暴躁 气炸 怼"},
	{"165", "玫瑰", "花 示爱 表白 浪漫 送你花 情人节"},
	{"119", "凋谢", "花谢 失恋 难过 枯萎 心碎 凉了"},
	{"159", "点赞", "赞 认同 好棒 good like 大拇指 顶"},
	{"164", "握手", "合作 你好 商务 hello deal 成交 友好"},
	{"163", "抱拳", "谢谢 失敬 江湖 承让 拜托 有礼"},
	{"169", "ok", "好的 收到 没问题 okay 行 可以 懂了"},
	{"174", "拳头", "加油 干 冲 fight 力量 击拳 硬气"},
	{"191", "鞭炮", "过年 喜庆 爆竹 春节 噼里啪啦 红"},
	{"258", "烟花", "庆典 漂亮 新年 嘭 绽放 节日快乐"},
}

// Exact name, then mutual substring of name, then description containment, then the best fuzzy match
func stickerByName(name string) (sticker, bool) {
	query := strings.TrimSpace(name)
	if query == "" {
		return sticker{}, false
	}
	for _, s := range stickers {
		if s.Name == query {
			return s, true
		}
	}
	for _, s := range stickers {
		if strings.Contains(s.Name, query) || strings.Contains(query, s.Name) {
			return s, true
		}
	}
	for _, s := range stickers {
		if strings.Contains(s.Description, query) {
			return s, true
		}
	}
	if matches := searchStickers(query, 1); len(matches) > 0 {
		return matches[0], true
	}
	return sticker{}, false
}

// Sticker whose id equals the trimmed id
func stickerByID(id string) (sticker, bool) {
	id = strings.TrimSpace(id)
	for _, s := range stickers {
		if id != "" && s.ID == id {
			return s, true
		}
	}
	return sticker{}, false
}

func randomSticker() sticker { return stickers[rand.IntN(len(stickers))] }

// NFKC folded, trimmed and lowercased text
func normalizeText(raw string) string {
	return strings.ToLower(strings.TrimSpace(norm.NFKC.String(raw)))
}

// Normalized text with whitespace and common punctuation removed
func compactText(raw string) []rune {
	var out []rune
	for _, r := range normalizeText(raw) {
		if unicode.IsSpace(r) || strings.ContainsRune("　-_·.,，。!！?？\"“”'‘’、/\\", r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Share of needle runes found in haystack counted as multisets
func multisetHitRatio(needle, haystack []rune) float64 {
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

// Jaccard similarity of the rune bigram sets
func bigramJaccard(a, b []rune) float64 {
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	grams := func(x []rune) map[string]bool {
		set := map[string]bool{}
		for i := 0; i+1 < len(x); i++ {
			set[string(x[i:i+2])] = true
		}
		return set
	}
	setA, setB := grams(a), grams(b)
	inter := 0
	for g := range setA {
		if setB[g] {
			inter++
		}
	}
	return float64(inter) / float64(len(setA)+len(setB)-inter)
}

// Share of needle matched in order as a subsequence of haystack
func subsequenceRatio(needle, haystack []rune) float64 {
	if len(needle) == 0 {
		return 0
	}
	j := 0
	for _, r := range haystack {
		if j < len(needle) && r == needle[j] {
			j++
		}
	}
	return float64(j) / float64(len(needle))
}

// Best match score of a query against one field from exact, substring, prefix and fuzzy signals
func scoreField(haystack, query string) float64 {
	hay, q := normalizeText(haystack), normalizeText(query)
	if hay == "" || q == "" {
		return 0
	}
	hayC, qC := compactText(haystack), compactText(query)
	qLen := utf8.RuneCountInString(q)
	best := max(multisetHitRatio(qC, hayC)*62, bigramJaccard(qC, hayC)*58, subsequenceRatio(qC, hayC)*52)
	if hay == q {
		best = max(best, 100)
	}
	if strings.Contains(hay, q) {
		best = max(best, float64(92+min(6, qLen)))
		if qLen == 1 {
			best = max(best, 68)
		}
	}
	if qLen >= 2 && strings.HasPrefix(hay, q) {
		best = max(best, 88)
	}
	if len(qC) > 0 && strings.Contains(string(hayC), string(qC)) {
		best = max(best, 86)
	}
	return best
}

// Builtin stickers ranked by fuzzy match on name, description and id, catalogue order for an empty query
func searchStickers(query string, limit int) []sticker {
	if limit == 0 {
		limit = 10
	}
	limit = max(1, min(500, limit))
	qNorm := normalizeText(query)
	if qNorm == "" {
		return append([]sticker(nil), stickers[:min(limit, len(stickers))]...)
	}
	type scored struct {
		score float64
		s     sticker
	}
	ranked := make([]scored, 0, len(stickers))
	for _, s := range stickers {
		idScore := 0.0
		if sid := normalizeText(s.ID); sid == qNorm {
			idScore = 100
		} else if sid != "" && strings.Contains(sid, qNorm) {
			idScore = 84
		}
		ranked = append(ranked, scored{max(scoreField(s.Name, query), scoreField(s.Description, query)*0.88, idScore), s})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if top := ranked[0].score; top > 0 {
		floor := max(6, top*0.35)
		switch {
		case top >= 22:
			floor = 18
		case top >= 12:
			floor = max(10, top*0.5)
		}
		filtered := ranked[:0:0]
		for _, r := range ranked {
			if r.score >= floor {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) > 0 {
			ranked = filtered
		}
	}
	out := make([]sticker, 0, limit)
	for _, r := range ranked[:min(limit, len(ranked))] {
		out = append(out, r.s)
	}
	return out
}

// TIMFaceElem at index 0 whose data carries the compact JSON metadata of the sticker
func stickerElem(s sticker) msgElem {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		StickerID string `json:"sticker_id"`
		PackageID string `json:"package_id"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Formats   string `json:"formats"`
		Name      string `json:"name"`
	}{s.ID, stickerPackageID, 128, 128, "png", s.Name})
	return msgElem{Type: "TIMFaceElem", Content: msgContent{Data: strings.TrimRight(buf.String(), "\n")}}
}
