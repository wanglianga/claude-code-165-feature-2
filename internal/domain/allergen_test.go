package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestSuspectNutsOrDairy(t *testing.T) {
	cases := []struct {
		name      string
		category  string
		desc      string
		allergens []string
		wantHit   bool
		wantGroup []string
	}{
		{"中文品类含坚果", "坚果燕麦能量棒", "", nil, true, []string{AllergenGroupNut}},
		{"中文描述含乳制品", "当日面包", "含鲜奶油夹心", nil, true, []string{AllergenGroupDairy}},
		{"过敏原代码命中坚果", "面包", "", []string{"peanut"}, true, []string{AllergenGroupNut}},
		{"过敏原代码命中乳制品", "面包", "", []string{"milk"}, true, []string{AllergenGroupDairy}},
		{"同时命中两组", "核桃奶油蛋糕", "", nil, true, []string{AllergenGroupNut, AllergenGroupDairy}},
		{"普通面包不命中", "全麦吐司", "无添加", nil, false, nil},
		{"蛋糕不直接命中", "蛋糕", "", nil, false, nil},
		{"英文词边界", "donut 甜甜圈", "", nil, false, nil}, // donut 不应误中 nut
		{"英文单词命中", "mixed nuts snack", "", nil, true, []string{AllergenGroupNut}},
		{"无过敏原标记不命中", "吐司", "", []string{"none"}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit, groups := SuspectNutsOrDairy(tc.category, tc.desc, tc.allergens)
			if hit != tc.wantHit {
				t.Fatalf("hit=%v, want %v (groups=%v)", hit, tc.wantHit, groups)
			}
			if tc.wantHit && !reflect.DeepEqual(groups, tc.wantGroup) {
				t.Fatalf("groups=%v, want %v", groups, tc.wantGroup)
			}
		})
	}
}

func TestSplitRecipientTypes(t *testing.T) {
	types := []string{"community_kitchen", "children", "elderly_care", "allergy_high_risk", "shelter"}
	// 含坚果/乳制品：敏感类型被排除
	allowed, excluded := SplitRecipientTypes([]string{"nut", "milk"}, types)
	if !reflect.DeepEqual(allowed, []string{"community_kitchen", "shelter"}) {
		t.Fatalf("allowed=%v", allowed)
	}
	if !reflect.DeepEqual(excluded, []string{"children", "elderly_care", "allergy_high_risk"}) {
		t.Fatalf("excluded=%v", excluded)
	}
	// 无高风险过敏原：全部允许
	allowed, excluded = SplitRecipientTypes([]string{"gluten"}, types)
	if len(allowed) != len(types) || len(excluded) != 0 {
		t.Fatalf("gluten should not exclude: allowed=%v excluded=%v", allowed, excluded)
	}
	// 空过敏原：全部允许
	allowed, _ = SplitRecipientTypes(nil, types)
	if len(allowed) != len(types) {
		t.Fatalf("empty allergens should allow all: %v", allowed)
	}
}

func TestCheckReservationPublishBlocked(t *testing.T) {
	b := baseBatch()
	b.PublishBlocked = true
	err := CheckReservation(ReserveCheckInput{
		Batch: b, Charity: baseCharity(), RequestBoxes: 1,
		PlannedPickupAt: time.Now().Add(time.Hour), Now: time.Now(),
		LargeReleaseRatio: 0.5, LargeReleaseMaxWait: 3 * time.Hour,
	})
	if err == nil || err.Code != "PUBLISH_BLOCKED" || err.Status != 422 {
		t.Fatalf("expect 422 PUBLISH_BLOCKED, got %v", err)
	}
}

func TestCheckReservationAllergenRiskExcluded(t *testing.T) {
	b := baseBatch()
	b.Allergens = []string{"nut", "milk"}
	// 机构只服务儿童/老人等敏感人群 -> 整体排除
	c := baseCharity()
	c.RecipientTypes = []string{"children", "elderly_care"}
	err := CheckReservation(ReserveCheckInput{
		Batch: b, Charity: c, RequestBoxes: 1,
		PlannedPickupAt: time.Now().Add(time.Hour), Now: time.Now(),
		LargeReleaseRatio: 0.5, LargeReleaseMaxWait: 3 * time.Hour,
	})
	if err == nil || err.Code != "ALLERGEN_RISK_EXCLUDED" {
		t.Fatalf("expect ALLERGEN_RISK_EXCLUDED, got %v", err)
	}
	// 机构同时服务普通受助点 -> 仍可预约（敏感站点在配送计划环节排除）
	c.RecipientTypes = []string{"children", "community_kitchen"}
	err = CheckReservation(ReserveCheckInput{
		Batch: b, Charity: c, RequestBoxes: 1,
		PlannedPickupAt: time.Now().Add(time.Hour), Now: time.Now(),
		LargeReleaseRatio: 0.5, LargeReleaseMaxWait: 3 * time.Hour,
	})
	if err != nil {
		t.Fatalf("expect pass with mixed recipient types, got %v", err)
	}
	// 无高风险过敏原 -> 纯敏感机构也可预约
	b.Allergens = []string{"gluten"}
	c.RecipientTypes = []string{"children"}
	err = CheckReservation(ReserveCheckInput{
		Batch: b, Charity: c, RequestBoxes: 1,
		PlannedPickupAt: time.Now().Add(time.Hour), Now: time.Now(),
		LargeReleaseRatio: 0.5, LargeReleaseMaxWait: 3 * time.Hour,
	})
	if err != nil {
		t.Fatalf("expect pass without high-risk allergens, got %v", err)
	}
}
