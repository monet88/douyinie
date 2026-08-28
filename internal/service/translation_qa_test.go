package service_test

import (
	"errors"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

func TestMeaningFirstQAGate_ValidPairs(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	tests := []struct {
		name    string
		source  string
		target  string
		srcLang string
		tgtLang string
	}{
		{
			name:    "Standard Vietnamese with numbers and negation",
			source:  "请将温度调至25度，张伟说不要打开窗户。",
			target:  "Vui lòng điều chỉnh nhiệt độ đến 25 độ, Trương Vĩ nói không được mở cửa sổ.",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Standard English with brand and numbers",
			source:  "SUPOR电饭煲拥有3升容量，煮饭不粘锅。",
			target:  "The SUPOR rice cooker has a 3-liter capacity and does not stick to the pot.",
			srcLang: "zh",
			tgtLang: "en",
		},
		{
			name:    "Recipe step with number and negation in Vietnamese",
			source:  "步骤1：准备抹茶粉20克，不要加糖。",
			target:  "Bước 1: Chuẩn bị 20 gram bột matcha, đừng thêm đường.",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Affirmative without negation in English",
			source:  "今天天气很好。我们去公园散步吧。",
			target:  "The weather is very good today. Let's go for a walk in the park.",
			srcLang: "zh",
			tgtLang: "en",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := qa.ValidateSegment(tt.source, tt.target, tt.srcLang, tt.tgtLang)
			if !res.Passed {
				t.Fatalf("expected passed, got failed with violations: %v, err: %v", res.Violations, res.Err)
			}
			if res.Confidence < 0.9 {
				t.Errorf("expected high confidence >= 0.9, got %f", res.Confidence)
			}
		})
	}
}

func TestMeaningFirstQAGate_NumberCorruption(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	source := "剪刀以45度角剪掉枝条。"
	corruptedTarget := "Dùng kéo cắt cành cây ở góc 90 độ." // 45 -> 90 corrupted

	res := qa.ValidateSegment(source, corruptedTarget, "zh", "vi")
	if res.Passed {
		t.Fatalf("expected QA gate to reject corrupted number, but it passed")
	}
	if !errors.Is(res.Err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNumberCorrupted, got: %v", res.Err)
	}
}

func TestMeaningFirstQAGate_NegationInversion(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. Source negative -> Target affirmative (inverted)
	src1 := "请不要打开窗户。"
	tgt1 := "Hãy mở cửa sổ ra nhé." // "Don't open" -> "Please open"
	res1 := qa.ValidateSegment(src1, tgt1, "zh", "vi")
	if res1.Passed {
		t.Fatalf("expected QA gate to reject dropped negation, but it passed")
	}
	if !errors.Is(res1.Err, domain.ErrNegationInverted) {
		t.Errorf("expected ErrNegationInverted, got: %v", res1.Err)
	}

	// 2. Source affirmative -> Target negative (inverted)
	src2 := "我们去公园散步吧。"
	tgt2 := "We should not go to the park." // Affirmative -> Negative
	res2 := qa.ValidateSegment(src2, tgt2, "zh", "en")
	if res2.Passed {
		t.Fatalf("expected QA gate to reject added negation, but it passed")
	}
	if !errors.Is(res2.Err, domain.ErrNegationInverted) {
		t.Errorf("expected ErrNegationInverted, got: %v", res2.Err)
	}
}

func TestMeaningFirstQAGate_NameCorruption(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	source := "SUPOR电饭煲非常好用，张伟推荐的。"
	corruptedTarget := "Nồi cơm điện Panasonic rất tốt, người khác giới thiệu." // SUPOR & 张伟 missing

	res := qa.ValidateSegment(source, corruptedTarget, "zh", "vi")
	if res.Passed {
		t.Fatalf("expected QA gate to reject corrupted named entity, but it passed")
	}
	if !errors.Is(res.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted, got: %v", res.Err)
	}
}

func TestMeaningFirstQAGate_EmptyFact(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	source := "今天天气很好。"
	emptyTarget := "   "

	res := qa.ValidateSegment(source, emptyTarget, "zh", "vi")
	if res.Passed {
		t.Fatalf("expected QA gate to reject empty target, but it passed")
	}
	if !errors.Is(res.Err, domain.ErrFactCorrupted) {
		t.Errorf("expected ErrFactCorrupted, got: %v", res.Err)
	}
}
