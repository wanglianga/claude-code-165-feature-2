package domain

import (
	"sort"
	"strings"
)

// 高风险过敏原组：门店上报含这些成分的食品却未填写过敏原时，批次拦截、不得发布给公益机构。
const (
	RiskGroupNuts  = "nuts"  // 坚果（含花生等坚果籽类）
	RiskGroupDairy = "dairy" // 乳制品
)

// allergenAliases 将门店/机构常见写法（含中文）归一到标准过敏原编码。
var allergenAliases = map[string]string{
	"gluten": "gluten", "麸质": "gluten", "小麦": "gluten", "wheat": "gluten",
	"egg": "egg", "鸡蛋": "egg", "蛋": "egg", "eggs": "egg",
	"milk": "milk", "milk_powder": "milk", "dairy": "milk", "lactose": "milk",
	"whey": "milk", "casein": "milk", "butter": "milk", "cream": "milk",
	"cheese": "milk", "yogurt": "milk", "yoghurt": "milk",
	"牛奶": "milk", "奶": "milk", "奶类": "milk", "乳": "milk", "乳制品": "milk",
	"乳清": "milk", "乳糖": "milk", "奶粉": "milk", "奶油": "milk", "黄油": "milk",
	"芝士": "milk", "奶酪": "milk", "干酪": "milk", "起司": "milk", "酸奶": "milk",
	"nut": "nut", "nuts": "nut", "tree_nut": "nut", "tree-nut": "nut",
	"almond": "nut", "walnut": "nut", "cashew": "nut", "hazelnut": "nut",
	"pistachio": "nut", "macadamia": "nut", "peanut": "peanut", "peanuts": "peanut",
	"坚果": "nut", "杏仁": "nut", "核桃": "nut", "腰果": "nut", "榛子": "nut",
	"开心果": "nut", "夏威夷果": "nut", "碧根果": "nut", "花生": "peanut",
	"soy": "soy", "大豆": "soy", "黄豆": "soy",
	"fish": "fish", "鱼": "fish",
	"shellfish": "shellfish", "甲壳类": "shellfish", "虾": "shellfish", "蟹": "shellfish",
	"sesame": "sesame", "芝麻": "sesame",
	"none": "none", "无": "none", "无过敏原": "none",
}

// nutsGroup / dairyGroup 成员编码（归一化后）。
var nutsGroup = map[string]bool{"nut": true, "peanut": true}
var dairyGroup = map[string]bool{"milk": true}

// NormalizeAllergens 去空格、小写、中英文别名归一，去重并排序。
func NormalizeAllergens(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range raw {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if canon, ok := allergenAliases[a]; ok {
			a = canon
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// RiskGroupsFromAllergens 返回过敏原列表命中的高风险成分组（nuts/dairy）。
func RiskGroupsFromAllergens(allergens []string) []string {
	groups := map[string]bool{}
	for _, a := range NormalizeAllergens(allergens) {
		if nutsGroup[a] {
			groups[RiskGroupNuts] = true
		}
		if dairyGroup[a] {
			groups[RiskGroupDairy] = true
		}
	}
	var out []string
	// 固定顺序保证提示语与凭证稳定
	if groups[RiskGroupDairy] {
		out = append(out, RiskGroupDairy)
	}
	if groups[RiskGroupNuts] {
		out = append(out, RiskGroupNuts)
	}
	return out
}

// riskKeywords 从品类/描述文本推断高风险成分（门店未填过敏原时的拦截依据）。
var riskKeywords = map[string][]string{
	RiskGroupNuts: {
		"坚果", "花生", "杏仁", "核桃", "腰果", "榛", "开心果", "夏威夷果", "碧根果", "栗子", "板栗",
		"nut", "peanut", "almond", "walnut", "cashew", "hazelnut", "pistachio", "macadamia", "pecan", "chestnut",
	},
	RiskGroupDairy: {
		"牛奶", "奶", "乳", "芝士", "奶酪", "干酪", "起司", "黄油", "奶油", "酸奶", "奶粉",
		"milk", "dairy", "cheese", "butter", "cream", "yogurt", "yoghurt", "whey", "casein", "latte",
	},
}

// DetectRiskIngredients 在过敏原未填写时，根据品类与描述推断是否含坚果/乳制品。
// 命中关键词返回风险组（dairy 优先于 nuts，保证顺序稳定）。
func DetectRiskIngredients(category, description string, allergens []string, confirmed bool) []string {
	if confirmed && len(NormalizeAllergens(allergens)) > 0 {
		return RiskGroupsFromAllergens(allergens)
	}
	text := strings.ToLower(category + " " + description)
	var out []string
	for _, g := range []string{RiskGroupDairy, RiskGroupNuts} {
		for _, kw := range riskKeywords[g] {
			if strings.Contains(text, strings.ToLower(kw)) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// RiskGroupLabels 风险组的中文展示名。
func RiskGroupLabels(groups []string) []string {
	labels := map[string]string{
		RiskGroupNuts:  "坚果",
		RiskGroupDairy: "乳制品",
	}
	var out []string
	for _, g := range groups {
		if l, ok := labels[g]; ok {
			out = append(out, l)
		} else {
			out = append(out, g)
		}
	}
	return out
}

// CharityAllergenEligible 判断机构能否接收该批次：
// 批次不含坚果/乳制品时一律可接收；含高风险成分时，服务儿童/老人/过敏高风险
// 受助点的机构除非已完成过敏原分区储运备案（allergen_managed），否则排除。
func CharityAllergenEligible(b Batch, c Charity) bool {
	groups := RiskGroupsFromAllergens(b.Allergens)
	if len(groups) == 0 {
		return true
	}
	if c.HighRiskRecipients && !c.AllergenManaged {
		return false
	}
	return true
}
