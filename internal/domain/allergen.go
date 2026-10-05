package domain

import "strings"

// ---------- 过敏原风险规则 ----------
//
// 高风险过敏原分组：坚果(nut) 与 乳制品(dairy)。
// 门店上报的食品疑似含有这两类成分但未填写过敏原时，批次被拦截发布，
// 并生成门店补录任务；补录后按分组重算可接收机构与受助点类型。

// AllergenGroupNut / AllergenGroupDairy 是高风险过敏原分组标识。
const (
	AllergenGroupNut   = "nut"
	AllergenGroupDairy = "dairy"
)

// allergenKeywords 将归一化后的过敏原代码/中文关键词映射到风险分组。
// 覆盖：标准过敏原代码、常见别名、中文品名关键词。
var allergenKeywords = map[string]string{
	// 坚果类（含树坚果与花生）
	"nut": AllergenGroupNut, "nuts": AllergenGroupNut, "tree_nut": AllergenGroupNut,
	"peanut": AllergenGroupNut, "walnut": AllergenGroupNut, "almond": AllergenGroupNut,
	"cashew": AllergenGroupNut, "hazelnut": AllergenGroupNut, "pistachio": AllergenGroupNut,
	"pecan": AllergenGroupNut, "macadamia": AllergenGroupNut, "chestnut": AllergenGroupNut,
	"pine_nut": AllergenGroupNut, "sesame": AllergenGroupNut,
	"坚果": AllergenGroupNut, "核桃": AllergenGroupNut, "杏仁": AllergenGroupNut,
	"腰果": AllergenGroupNut, "榛子": AllergenGroupNut, "榛果": AllergenGroupNut,
	"花生": AllergenGroupNut, "开心果": AllergenGroupNut, "碧根果": AllergenGroupNut,
	"夏威夷果": AllergenGroupNut, "巴旦木": AllergenGroupNut, "扁桃": AllergenGroupNut,
	"松子": AllergenGroupNut, "板栗": AllergenGroupNut, "栗子": AllergenGroupNut,
	"芝麻": AllergenGroupNut, "果仁": AllergenGroupNut,
	// 乳制品
	"milk": AllergenGroupDairy, "dairy": AllergenGroupDairy, "cheese": AllergenGroupDairy,
	"butter": AllergenGroupDairy, "cream": AllergenGroupDairy, "yogurt": AllergenGroupDairy,
	"whey": AllergenGroupDairy, "lactose": AllergenGroupDairy, "casein": AllergenGroupDairy,
	"乳": AllergenGroupDairy, "奶": AllergenGroupDairy, "芝士": AllergenGroupDairy,
	"黄油": AllergenGroupDairy, "乳酪": AllergenGroupDairy, "炼乳": AllergenGroupDairy,
	"酸奶": AllergenGroupDairy, "奶油": AllergenGroupDairy, "奶酪": AllergenGroupDairy,
	"奶粉": AllergenGroupDairy, "干酪": AllergenGroupDairy,
}

// 单字关键词参与子串匹配时容易误伤（如“乳”在“腐乳”中仍属合理命中，
// 但“奶”需避免误命中非食品词），这里约定：中文按子串匹配，英文按精确/归一化匹配。

// SensitiveRecipientTypes 是过敏敏感受助点类型：儿童、老人、过敏高风险人群。
// 批次含高风险过敏原时，这些受助点类型被排除在可发放范围之外。
var SensitiveRecipientTypes = map[string]bool{
	"children":          true, // 儿童
	"elderly_care":      true, // 老人
	"allergy_high_risk": true, // 过敏高风险
}

// normalizeAllergen 归一化过敏原代码：小写、去空白与连字符差异。
func normalizeAllergen(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// HighRiskGroups 计算过敏原列表命中的高风险分组（nut/dairy），去重且有序。
func HighRiskGroups(allergens []string) []string {
	set := map[string]bool{}
	for _, a := range allergens {
		norm := normalizeAllergen(a)
		if g, ok := allergenKeywords[norm]; ok {
			set[g] = true
			continue
		}
		// 中文关键词子串匹配（如“核桃仁”“鲜牛奶”）
		for kw, g := range allergenKeywords {
			if isASCII(kw) {
				continue
			}
			if strings.Contains(norm, kw) {
				set[g] = true
			}
		}
	}
	return orderedGroups(set)
}

// orderedGroups 按固定顺序（nut 在前、dairy 在后）输出分组，保证结果确定。
func orderedGroups(set map[string]bool) []string {
	var groups []string
	for _, g := range []string{AllergenGroupNut, AllergenGroupDairy} {
		if set[g] {
			groups = append(groups, g)
		}
	}
	return groups
}

// SuspectNutsOrDairy 在上报时检测食品是否疑似含坚果或乳制品：
// 综合品类、描述的中文/英文关键词与已填写的过敏原代码。
// 返回是否命中及命中的风险分组。
func SuspectNutsOrDairy(category, description string, allergens []string) (bool, []string) {
	set := map[string]bool{}
	for _, g := range HighRiskGroups(allergens) {
		set[g] = true
	}
	text := category + " " + description
	for kw, g := range allergenKeywords {
		if isASCII(kw) {
			// 英文关键词按词匹配（避免 "nut" 误中 "donut" 等）
			if containsWord(text, kw) {
				set[g] = true
			}
			continue
		}
		if strings.Contains(text, kw) {
			set[g] = true
		}
	}
	groups := orderedGroups(set)
	return len(groups) > 0, groups
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// containsWord 判断文本中是否出现独立的英文单词（空格/标点分隔）。
func containsWord(text, word string) bool {
	lower := strings.ToLower(text)
	idx := 0
	for {
		i := strings.Index(lower[idx:], word)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(word)
		leftOK := start == 0 || !isAlphaNum(lower[start-1])
		rightOK := end == len(lower) || !isAlphaNum(lower[end])
		if leftOK && rightOK {
			return true
		}
		idx = start + 1
	}
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// SplitRecipientTypes 按批次已确认过敏原拆分受助点类型：
// 批次命中高风险过敏原分组时，儿童/老人/过敏高风险等敏感类型被排除。
// 返回 (可接收类型, 被排除类型)。
func SplitRecipientTypes(allergens, recipientTypes []string) (allowed, excluded []string) {
	highRisk := len(HighRiskGroups(allergens)) > 0
	for _, t := range recipientTypes {
		if highRisk && SensitiveRecipientTypes[t] {
			excluded = append(excluded, t)
			continue
		}
		allowed = append(allowed, t)
	}
	return allowed, excluded
}
