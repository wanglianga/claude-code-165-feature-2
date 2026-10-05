package domain

import (
	"testing"
	"time"
)

func TestNormalizeAllergens(t *testing.T) {
	got := NormalizeAllergens([]string{" Milk ", "坚果", "坚果", "花生", "Gluten"})
	want := map[string]bool{"milk": true, "nut": true, "peanut": true, "gluten": true}
	if len(got) != 4 {
		t.Fatalf("expect 4 unique allergens, got %v", got)
	}
	for _, a := range got {
		if !want[a] {
			t.Fatalf("unexpected allergen %q in %v", a, got)
		}
	}
	if NormalizeAllergens([]string{"乳制品", "芝士"})[0] != "milk" {
		t.Fatal("dairy aliases should normalize to milk")
	}
}

func TestRiskGroupsFromAllergens(t *testing.T) {
	groups := RiskGroupsFromAllergens([]string{"milk", "egg"})
	if len(groups) != 1 || groups[0] != RiskGroupDairy {
		t.Fatalf("expect [dairy], got %v", groups)
	}
	groups = RiskGroupsFromAllergens([]string{"花生", "杏仁"})
	if len(groups) != 1 || groups[0] != RiskGroupNuts {
		t.Fatalf("expect [nuts], got %v", groups)
	}
	groups = RiskGroupsFromAllergens([]string{"牛奶", "坚果"})
	if len(groups) != 2 || groups[0] != RiskGroupDairy || groups[1] != RiskGroupNuts {
		t.Fatalf("expect [dairy nuts], got %v", groups)
	}
	if len(RiskGroupsFromAllergens([]string{"egg", "gluten"})) != 0 {
		t.Fatal("egg/gluten are not high-risk groups")
	}
}

func TestDetectRiskIngredients(t *testing.T) {
	// 未确认过敏原时按文本推断
	groups := DetectRiskIngredients("坚果欧包", "含核桃与杏仁片", nil, false)
	if len(groups) != 1 || groups[0] != RiskGroupNuts {
		t.Fatalf("expect nuts detected from description, got %v", groups)
	}
	groups = DetectRiskIngredients("奶油蛋糕", "新西兰黄油与芝士", nil, false)
	if len(groups) != 1 || groups[0] != RiskGroupDairy {
		t.Fatalf("expect dairy detected, got %v", groups)
	}
	groups = DetectRiskIngredients("法棍", "原味", nil, false)
	if len(groups) != 0 {
		t.Fatalf("expect no risk, got %v", groups)
	}
	// 已确认时以过敏原列表为准
	groups = DetectRiskIngredients("坚果欧包", "", []string{"none"}, true)
	if len(groups) != 0 {
		t.Fatalf("confirmed none should yield no risk, got %v", groups)
	}
}

func TestCharityAllergenEligible(t *testing.T) {
	nutBatch := Batch{Allergens: []string{"nut"}}
	plainBatch := Batch{Allergens: []string{"egg"}}
	highRisk := Charity{HighRiskRecipients: true, AllergenManaged: false}
	managed := Charity{HighRiskRecipients: true, AllergenManaged: true}
	normal := Charity{HighRiskRecipients: false}
	if CharityAllergenEligible(nutBatch, highRisk) {
		t.Fatal("high-risk recipient charity without management must be excluded")
	}
	if !CharityAllergenEligible(nutBatch, managed) {
		t.Fatal("allergen-managed charity remains eligible")
	}
	if !CharityAllergenEligible(nutBatch, normal) {
		t.Fatal("ordinary charity eligible for nut batch")
	}
	if !CharityAllergenEligible(plainBatch, highRisk) {
		t.Fatal("non-nut/dairy batch is always eligible")
	}
}

func TestCheckReservationHeldBatch(t *testing.T) {
	now := time.Now()
	b := baseBatch()
	b.AllergenHeld = true
	b.AllergenConfirmed = false
	err := CheckReservation(ReserveCheckInput{
		Batch: b, Charity: baseCharity(), RequestBoxes: 1,
		PlannedPickupAt: now.Add(time.Hour), Now: now,
		LargeReleaseRatio: 0.5, LargeReleaseMaxWait: 3 * time.Hour,
		AllergenEligible: true,
	})
	if err == nil || err.Status != 422 {
		t.Fatalf("held batch must not be reservable, got %v", err)
	}
}

func TestCheckReservationAllergenExclusion(t *testing.T) {
	now := time.Now()
	b := baseBatch()
	b.Allergens = []string{"milk", "nut"}
	err := CheckReservation(ReserveCheckInput{
		Batch: b, Charity: baseCharity(), RequestBoxes: 1,
		PlannedPickupAt: now.Add(time.Hour), Now: now,
		LargeReleaseRatio: 0.5, LargeReleaseMaxWait: 3 * time.Hour,
		AllergenEligible: false,
	})
	if err == nil || err.Status != 422 {
		t.Fatalf("excluded charity must be rejected 422, got %v", err)
	}
}
