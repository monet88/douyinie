package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
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
		{
			name:    "Lexical compound 不 (不透明度) does not require target negation",
			source:  "点击不透明度，拉到一百",
			target:  "nhấn vào Độ mờ, kéo lên 100",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Lexical compound 不 (不锈钢) does not require target negation",
			source:  "这个是不锈钢材质",
			target:  "cái này là chất liệu inox",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Lexical compound 不 (不一定) translated without target negation",
			source:  "明天不一定下雨",
			target:  "Ngày mai có lẽ mưa",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Lexical compound 不 (不可避免) translated as affirmative word",
			source:  "这是不可避免的",
			target:  "Điều này là tất yếu",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Lexical compound 不 (不可避免) translated with target negation",
			source:  "这是不可避免的",
			target:  "Điều này là không thể tránh khỏi",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Short OCR non-word token TM dropped without penalty",
			source:  "最新款式 TM",
			target:  "Kiểu dáng mới nhất",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Long alphanumeric code dropped without penalty",
			source:  "型号SO50L207",
			target:  "Model 50 207",
			srcLang: "zh",
			tgtLang: "vi",
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

func TestMeaningFirstQAGate_AcceptsEnglishFrequencyCountWords(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	if nums := service.ExtractNumbers("tie it twice", "en"); !nums["2"] {
		t.Fatalf("expected ExtractNumbers to contain 2, got %v", nums)
	}

	source := "交叉绑提手两次就能滴水不漏"
	target := "Cross the handles and tie it twice so it will not leak."
	res := qa.ValidateSegment(source, target, "zh", "en")
	if !res.Passed {
		t.Fatalf("expected twice to satisfy 两次, got violations: %v, err: %v", res.Violations, res.Err)
	}
}

func TestExtractNumbers_ChineseAspectualYiNotQuantity(t *testing.T) {
	// Aspectual/action 一+verb, adverbials, ordinals, labels: no hard 1.
	for _, src := range []string{
		"手机手电筒一照", "手 机 手 电 筒 一 照",
		"当你想要看洗浴用品余量只需要用手机手电筒一照",
		"一拉", "一 拉", "一剥", "一 剥", "一扯", "一 扯",
		"感受一下这期", "人物一直輸出", "都不一样", "都不一樣",
		"在了一起", "准备一些音乐", "用力一切不破",
		"放一点补充", "时间一分一秒的流失",
		"话一和话六", "第一件事先去化妆", "第十名",
	} {
		if nums := service.ExtractNumbers(src, "zh"); nums["1"] {
			t.Errorf("expected no hard 1 in %q, got %v", src, nums)
		}
	}
}

func TestExtractNumbers_ChineseExplicitQuantity(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"交叉绑提手两次", "2"},
		{"交叉绑提手两 次", "2"},
		{"微波炉高火二十秒", "20"},
		{"微波炉高火二 十 秒", "20"},
		{"只需要一个衣架", "1"},
		{"只需要一 个 衣 架", "1"},
		{"温度调到二十五度", "25"},
		{"温度调到二 十 五 度", "25"},
		{"三天三夜", "3"},
		{"十分钟吃饭", "10"},
		{"这还真是八小时工作制", "8"},
		{"我呼气得有一斤多", "1"},
		{"二斤", "2"},
		{"只需要一個一架", "1"},
		{"大家三种技巧", "3"},
		{"七种画面处理方式", "7"},
		{"複製一層畫面", "1"},
		{"贏得了一粒點球", "1"},
	}
	for _, tc := range cases {
		if nums := service.ExtractNumbers(tc.src, "zh"); !nums[tc.want] {
			t.Errorf("expected %s in %q, got %v", tc.want, tc.src, nums)
		}
	}

	// Spaced enumerations stay separate numerals without quantity context.
	if nums := service.ExtractNumbers("一 二 三", "zh"); len(nums) != 0 {
		t.Errorf("expected no hard numbers in enumeration, got %v", nums)
	}
}

func TestMeaningFirstQAGate_ChineseAspectualYiPasses(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	res := qa.ValidateSegment(
		"当你想要看洗浴用品余量只需要用手机手电筒一照",
		"dùng đèn pin điện thoại để chiếu vào",
		"zh", "vi",
	)
	if !res.Passed {
		t.Fatalf("expected aspectual 一照 to pass number gate, got violations=%v err=%v", res.Violations, res.Err)
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

	// Genuine sentence-level prohibition inverted
	srcProhibit := "务必别打开窗户"
	tgtProhibit := "Hãy mở cửa sổ" // "Don't open" -> "Please open"
	resProhibit := qa.ValidateSegment(srcProhibit, tgtProhibit, "zh", "vi")
	if resProhibit.Passed {
		t.Fatalf("expected QA gate to reject dropped prohibition, but it passed")
	}
	if !errors.Is(resProhibit.Err, domain.ErrNegationInverted) {
		t.Errorf("expected ErrNegationInverted, got: %v", resProhibit.Err)
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

	// 3. Proper-name & transliteration controls with standalone "Phi" (should NOT trigger negation inversion)
	for _, nameCase := range []struct {
		src string
		tgt string
	}{
		{"王菲在北京开演唱会", "Vương Phi tổ chức hòa nhạc tại Bắc Kinh."},
		{"黄飞鸿是武术大师", "Hoàng Phi Hồng là bậc thầy võ thuật."},
		{"刘德华在电影里表演", "Lưu Đức Hoa biểu diễn trong phim điện ảnh."},
		{"张飞是蜀汉名将", "Trương Phi là danh tướng Thục Hán."},
		{"燕飞在天空中自由翱翔", "Yến phi tự do lượn trên bầu trời."},
		{"陈飞宇参演了新电影", "Trần Phi Vũ tham gia phim mới."},
		{"这是李菲的个人作品", "Đây là tác phẩm cá nhân của Lý Phi."},
		{"飞鸟在森林里唱歌", "Phi điểu ca hát trong rừng rậm."},
	} {
		res := qa.ValidateSegment(nameCase.src, nameCase.tgt, "zh", "vi")
		if !res.Passed {
			t.Errorf("expected proper name / non-negating token with 'Phi' in %q -> %q to pass QA, but failed: %v (err: %v)",
				nameCase.src, nameCase.tgt, res.Violations, res.Err)
		}
	}

	// 4. Genuine Vietnamese negation controls (MUST REMAIN FAIL-CLOSED)
	// 4a: Source affirmative translated with sentence-level negator -> MUST FAIL
	for _, falseNegCase := range []struct {
		src string
		tgt string
	}{
		{"我们去吃晚餐", "Chúng ta không đi ăn tối."},
		{"大家一起看电影", "Mọi người chưa xem phim."},
		{"你可以打开大门", "Bạn đừng mở cửa lớn."},
		{"这里允许吸烟", "Ở đây cấm hút thuốc."},
		{"该行为符合法律规定", "Hành vi đó là phi pháp."},
		{"他的解释非常合理", "Lời giải thích của anh ấy là vô lý."},
	} {
		res := qa.ValidateSegment(falseNegCase.src, falseNegCase.tgt, "zh", "vi")
		if res.Passed {
			t.Errorf("expected added negation in %q -> %q to FAIL QA, but passed", falseNegCase.src, falseNegCase.tgt)
		}
		if !errors.Is(res.Err, domain.ErrNegationInverted) {
			t.Errorf("expected ErrNegationInverted for %q -> %q, got: %v", falseNegCase.src, falseNegCase.tgt, res.Err)
		}
	}

	// 4b: Source negative translated affirmatively -> MUST FAIL
	for _, droppedNegCase := range []struct {
		src string
		tgt string
	}{
		{"他不喜欢这个礼物", "Anh ấy thích món quà này."},
		{"我们还没有完成任务", "Chúng tôi đã hoàn thành nhiệm vụ."},
		{"严禁在此处拍照", "Được phép chụp ảnh ở đây."},
		{"不要随意走动", "Hãy đi lại thoải mái."},
	} {
		res := qa.ValidateSegment(droppedNegCase.src, droppedNegCase.tgt, "zh", "vi")
		if res.Passed {
			t.Errorf("expected dropped negation in %q -> %q to FAIL QA, but passed", droppedNegCase.src, droppedNegCase.tgt)
		}
		if !errors.Is(res.Err, domain.ErrNegationInverted) {
			t.Errorf("expected ErrNegationInverted for %q -> %q, got: %v", droppedNegCase.src, droppedNegCase.tgt, res.Err)
		}
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

	// Diacritic-decorated latin transliterations of protected ASCII names (e.g. VörtexBrand -> VortexBrand)
	// must pass QA as valid matching names.
	resDiacritic := qa.ValidateSegment("VortexBrand", "VörtexBrand", "zh", "vi")
	if !resDiacritic.Passed {
		t.Fatalf("expected diacritic-decorated transliteration 'VörtexBrand' to match 'VortexBrand', got violations: %v", resDiacritic.Violations)
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

func TestMeaningFirstQAGate_AllCapsLexicalWordsNotProtected(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// "TOTAL DAMAGE CAE" translated naturally to Vietnamese where TOTAL and DAMAGE
	// are translated as "Tổng thiệt hại" but the brand SUPOR is preserved.
	source := "TOTAL DAMAGE SUPOR"
	naturalTarget := "Tổng thiệt hại SUPOR"

	res := qa.ValidateSegment(source, naturalTarget, "en", "vi")
	if !res.Passed {
		t.Fatalf("expected natural translation of 'TOTAL DAMAGE SUPOR' to pass, but got violations: %v, err: %v", res.Violations, res.Err)
	}

	// If the brand SUPOR is dropped/corrupted, it MUST still fail
	corruptedTarget := "Tổng thiệt hại hoàn toàn"
	resCorrupted := qa.ValidateSegment(source, corruptedTarget, "en", "vi")
	if resCorrupted.Passed {
		t.Fatalf("expected dropped token SUPOR to fail QA gate, but it passed")
	}
	if !errors.Is(resCorrupted.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped SUPOR, got: %v", resCorrupted.Err)
	}
}

func TestMeaningFirstQAGate_ProtectedASCIIFormsRetained(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. Digit-bearing token (MP4) preserved
	resMP4 := qa.ValidateSegment("Video xuất ra định dạng MP4.", "Video exported in MP4 format.", "vi", "en")
	if !resMP4.Passed {
		t.Fatalf("expected MP4 preserved to pass, got: %v", resMP4.Err)
	}
	// Target preserves numeric fact '4' while dropping the letters of the protected ASCII token 'MP4'.
	// Must fail closed with ErrNameCorrupted rather than tripping on a missing number.
	resMP4Drop := qa.ValidateSegment("Video xuất ra định dạng MP4.", "Video exported in 4 format.", "vi", "en")
	if resMP4Drop.Passed {
		t.Fatalf("expected dropped MP4 to fail, but passed")
	}
	if !errors.Is(resMP4Drop.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped MP4 token, got: %v", resMP4Drop.Err)
	}

	// 2. Digit-bearing token (4K) preserved
	res4K := qa.ValidateSegment("Hỗ trợ xuất video 4K siêu nét.", "Supports 4K video export.", "vi", "en")
	if !res4K.Passed {
		t.Fatalf("expected 4K preserved to pass, got: %v", res4K.Err)
	}
	// Target preserves numeric fact '4' while dropping '4K' token.
	res4KDrop := qa.ValidateSegment("Hỗ trợ xuất video 4K siêu nét.", "Supports ultra high resolution 4 video export.", "vi", "en")
	if res4KDrop.Passed {
		t.Fatalf("expected dropped 4K to fail, but passed")
	}
	if !errors.Is(res4KDrop.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped 4K token, got: %v", res4KDrop.Err)
	}

	// 3. Known entity brand (SUPOR) remains protected even though >= 4 chars all-caps
	resSupor := qa.ValidateSegment("SUPOR电饭煲", "Nồi cơm điện SUPOR", "zh", "vi")
	if !resSupor.Passed {
		t.Fatalf("expected SUPOR preserved to pass, got: %v", resSupor.Err)
	}
	resSuporDrop := qa.ValidateSegment("SUPOR电饭煲", "Nồi cơm điện cao cấp", "zh", "vi")
	if resSuporDrop.Passed {
		t.Fatalf("expected dropped SUPOR to fail, but passed")
	}

	// 4. Mixed-case brand (iPhone) preserved
	resIPhone := qa.ValidateSegment("使用iPhone拍摄", "Quay bằng iPhone", "zh", "vi")
	if !resIPhone.Passed {
		t.Fatalf("expected iPhone preserved to pass, got: %v", resIPhone.Err)
	}
	resIPhoneDrop := qa.ValidateSegment("使用iPhone拍摄", "Quay bằng điện thoại", "zh", "vi")
	if resIPhoneDrop.Passed {
		t.Fatalf("expected dropped iPhone to fail, but passed")
	}
}

func TestMeaningFirstQAGate_ThreeDigitGroupNumberEquivalence(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. Observed pair: "6 909999520" -> "6 909 999 520" passes
	resObserved := qa.ValidateSegment("6 909999520", "6 909 999 520", "zh", "vi")
	if !resObserved.Passed {
		t.Fatalf("expected observed pair '6 909999520' -> '6 909 999 520' to pass, got violations: %v, err: %v", resObserved.Violations, resObserved.Err)
	}

	// 2. Exact 9-digit sequence: "909999520" -> "909 999 520" passes
	resGrouped := qa.ValidateSegment("909999520", "909 999 520", "zh", "vi")
	if !resGrouped.Passed {
		t.Fatalf("expected '909999520' -> '909 999 520' to pass, got violations: %v, err: %v", resGrouped.Violations, resGrouped.Err)
	}

	// 3. Merged direction: "909 999 520" -> "909999520" passes
	resMerged := qa.ValidateSegment("909 999 520", "909999520", "zh", "vi")
	if !resMerged.Passed {
		t.Fatalf("expected '909 999 520' -> '909999520' to pass, got violations: %v, err: %v", resMerged.Violations, resMerged.Err)
	}

	// 4. Changed final digit fails: "909999520" -> "909 999 521"
	resDiffFinal := qa.ValidateSegment("909999520", "909 999 521", "zh", "vi")
	if resDiffFinal.Passed {
		t.Fatalf("expected changed final digit '909999520' -> '909 999 521' to fail, but it passed")
	}
	if !errors.Is(resDiffFinal.Err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNumberCorrupted, got: %v", resDiffFinal.Err)
	}

	// 5. Short numbers: "10 20" -> "1020" still fails
	resShort := qa.ValidateSegment("10 20", "1020", "zh", "vi")
	if resShort.Passed {
		t.Fatalf("expected '10 20' -> '1020' to fail, but it passed")
	}
	if !errors.Is(resShort.Err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNumberCorrupted, got: %v", resShort.Err)
	}
}

func TestMeaningFirstQAGate_PlainTitleCaseNotProtected(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. Plain TitleCase: Shift -> Chuyển đổi PASS
	resShift := qa.ValidateSegment("Shift", "Chuyển đổi", "zh", "vi")
	if !resShift.Passed {
		t.Fatalf("expected 'Shift' -> 'Chuyển đổi' to pass, got violations: %v, err: %v", resShift.Violations, resShift.Err)
	}

	// 2. Plain TitleCase: Total -> Tổng PASS
	resTotal := qa.ValidateSegment("Total", "Tổng", "en", "vi")
	if !resTotal.Passed {
		t.Fatalf("expected 'Total' -> 'Tổng' to pass, got violations: %v, err: %v", resTotal.Violations, resTotal.Err)
	}

	// 3. Dropping iPhone or YouTube FAIL
	resIPhoneDrop := qa.ValidateSegment("iPhone", "Điện thoại", "en", "vi")
	if resIPhoneDrop.Passed {
		t.Fatalf("expected dropping iPhone to fail QA gate, but passed")
	}
	if !errors.Is(resIPhoneDrop.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped iPhone, got: %v", resIPhoneDrop.Err)
	}

	resYouTubeDrop := qa.ValidateSegment("YouTube", "Trang video", "en", "vi")
	if resYouTubeDrop.Passed {
		t.Fatalf("expected dropping YouTube to fail QA gate, but passed")
	}
	if !errors.Is(resYouTubeDrop.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped YouTube, got: %v", resYouTubeDrop.Err)
	}

	// 4. Dropping SUPOR or Matcha FAIL
	resSuporDrop := qa.ValidateSegment("SUPOR", "Nồi cơm điện", "zh", "vi")
	if resSuporDrop.Passed {
		t.Fatalf("expected dropping SUPOR to fail QA gate, but passed")
	}
	if !errors.Is(resSuporDrop.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped SUPOR, got: %v", resSuporDrop.Err)
	}

	resMatchaDrop := qa.ValidateSegment("Matcha", "Trà xanh", "zh", "vi")
	if resMatchaDrop.Passed {
		t.Fatalf("expected dropping Matcha to fail QA gate, but passed")
	}
	if !errors.Is(resMatchaDrop.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for dropped Matcha, got: %v", resMatchaDrop.Err)
	}
}

func TestMeaningFirstQAGate_QuantityCompoundTokensNotProtectedAsNames(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. "3MINUTE" -> "3 phút" PASS (exact quantity token)
	resMinute := qa.ValidateSegment("3MINUTE", "3 phút", "zh", "vi")
	if !resMinute.Passed {
		t.Fatalf("expected '3MINUTE' -> '3 phút' to pass, got violations: %v, err: %v", resMinute.Violations, resMinute.Err)
	}
	// 2. OCR-tolerant quantity tokens against whitelisted unit words (edit distance <= 1):
	// 2a. Deletion: "3DY" (deletion of 'A' from DAY) -> "3 ngày" PASS
	resDyVi := qa.ValidateSegment("3DY", "3 ngày", "zh", "vi")
	if !resDyVi.Passed {
		t.Fatalf("expected '3DY' -> '3 ngày' to pass, got violations: %v, err: %v", resDyVi.Violations, resDyVi.Err)
	}
	resDyEn := qa.ValidateSegment("3DY", "3 days", "zh", "en")
	if !resDyEn.Passed {
		t.Fatalf("expected '3DY' -> '3 days' to pass, got violations: %v, err: %v", resDyEn.Violations, resDyEn.Err)
	}
	// 2b. Deletion: "3MINUE" (deletion of 'T' from MINUTE) -> "3 phút" PASS
	resMinue := qa.ValidateSegment("3MINUE", "3 phút", "zh", "vi")
	if !resMinue.Passed {
		t.Fatalf("expected '3MINUE' -> '3 phút' to pass, got violations: %v, err: %v", resMinue.Violations, resMinue.Err)
	}
	// 2c. Insertion: "5MINUTEE" (insertion of 'E' in MINUTE) -> "5 phút" PASS
	resMinutee := qa.ValidateSegment("5MINUTEE", "5 phút", "zh", "vi")
	if !resMinutee.Passed {
		t.Fatalf("expected '5MINUTEE' -> '5 phút' to pass, got violations: %v, err: %v", resMinutee.Violations, resMinutee.Err)
	}
	// 2d. Substitution: "3DEY" (substitution 'A'->'E' in DAY) -> "3 ngày" PASS
	resDey := qa.ValidateSegment("3DEY", "3 ngày", "zh", "vi")
	if !resDey.Passed {
		t.Fatalf("expected '3DEY' -> '3 ngày' to pass, got violations: %v, err: %v", resDey.Violations, resDey.Err)
	}
	// 3. "20KG" and "500ML" PASS
	resKg := qa.ValidateSegment("20KG", "20 kg", "en", "vi")
	if !resKg.Passed {
		t.Fatalf("expected '20KG' -> '20 kg' to pass, got violations: %v, err: %v", resKg.Violations, resKg.Err)
	}
	resMl := qa.ValidateSegment("500ML", "500 ml", "en", "vi")
	if !resMl.Passed {
		t.Fatalf("expected '500ML' -> '500 ml' to pass, got violations: %v, err: %v", resMl.Violations, resMl.Err)
	}

	// 4. "3MINUE" -> "4 phút" FAIL number preservation
	resMinueCorrupt := qa.ValidateSegment("3MINUE", "4 phút", "zh", "vi")
	if resMinueCorrupt.Passed {
		t.Fatalf("expected '3MINUE' -> '4 phút' to fail number preservation, but passed")
	}
	if !errors.Is(resMinueCorrupt.Err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNumberCorrupted for '3MINUE' -> '4 phút', got: %v", resMinueCorrupt.Err)
	}

	// 5. Arbitrary "3BRAND" and short-unit typos (e.g. 3KGG) stay protected as brand/name
	resBrandTrans := qa.ValidateSegment("3BRAND", "3 nhãn hiệu", "zh", "vi")
	if resBrandTrans.Passed {
		t.Fatalf("expected translated '3BRAND' -> '3 nhãn hiệu' to fail name preservation, but passed")
	}
	if !errors.Is(resBrandTrans.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for translated '3BRAND', got: %v", resBrandTrans.Err)
	}

	resBrandDrop := qa.ValidateSegment("3BRAND", "nhãn hiệu", "zh", "vi")
	if resBrandDrop.Passed {
		t.Fatalf("expected dropped '3BRAND' -> 'nhãn hiệu' to fail, but passed")
	}
	if !errors.Is(resBrandDrop.Err, domain.ErrNameCorrupted) && !errors.Is(resBrandDrop.Err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNameCorrupted or ErrNumberCorrupted for dropped '3BRAND', got: %v", resBrandDrop.Err)
	}

	// Short unit typo like "3KGG" or "3KGS" must not be fuzzied into a quantity token
	resKgg := qa.ValidateSegment("3KGG", "3 kg", "zh", "vi")
	if resKgg.Passed {
		t.Fatalf("expected short-unit typo '3KGG' -> '3 kg' to fail name preservation, but passed")
	}
	if !errors.Is(resKgg.Err, domain.ErrNameCorrupted) {
		t.Errorf("expected ErrNameCorrupted for short-unit typo '3KGG', got: %v", resKgg.Err)
	}
	// 6. Protected tokens: 4K, MP4, H264, iPhone, YouTube, SUPOR stay protected
	for _, protected := range []struct {
		src string
		tgt string
		tok string
	}{
		{"Video 4K", "Video 4 siêu nét", "4K"},
		{"Định dạng MP4", "Định dạng 4 video", "MP4"},
		{"Chuẩn nén H264", "Chuẩn nén 264 video", "H264"},
		{"Điện thoại iPhone", "Điện thoại thông minh", "iPhone"},
		{"Kênh YouTube", "Kênh video", "YouTube"},
		{"SUPOR电饭煲", "Nồi cơm điện", "SUPOR"},
	} {
		res := qa.ValidateSegment(protected.src, protected.tgt, "zh", "vi")
		if res.Passed {
			t.Fatalf("expected dropping '%s' in '%s' -> '%s' to fail name preservation, but passed", protected.tok, protected.src, protected.tgt)
		}
		if !errors.Is(res.Err, domain.ErrNameCorrupted) {
			t.Errorf("expected ErrNameCorrupted for '%s', got: %v", protected.tok, res.Err)
		}
	}

	// 7. Dropping the number "3" entirely from "3MINUTE" fails number check
	resDropNum := qa.ValidateSegment("3MINUTE", "vài phút", "zh", "vi")
	if resDropNum.Passed {
		t.Fatalf("expected dropping number in '3MINUTE' -> 'vài phút' to fail, but passed")
	}
	if !errors.Is(resDropNum.Err, domain.ErrNumberCorrupted) {
		t.Errorf("expected ErrNumberCorrupted, got: %v", resDropNum.Err)
	}
}
func TestMeaningFirstQAGate_QuestionTagNegationNotPolarityInverted(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// Case 1: "他应该是喜欢你不" ends with question tag where "不" is an interrogative modal particle ("right?", "isn't he?").
	// Translating to affirmative "Anh ấy chắc là thích cậu" must pass QA and NOT trigger negation polarity inversion.
	src := "他应该是喜欢你不"
	tgt := "Anh ấy chắc là thích cậu."
	res := qa.ValidateSegment(src, tgt, "zh", "vi")
	if !res.Passed {
		t.Fatalf("expected question particle '不' in %q not to trigger negation inversion, got: %v (err: %v)", src, res.Violations, res.Err)
	}

	// Case 2 (The Authoritative 7679392272936389915 Segment 21 shape):
	// In unpunctuated ASR streams, a clause-final question tag with 不 is followed immediately
	// by a new clause (e.g. "他应该是喜欢你不你长得真帅").
	// The interrogative tag 不 must not be treated as semantic negation.
	srcUnpunct := "他应该是喜欢你不你长得真帅"
	tgtUnpunct := "Anh ấy chắc là thích cậu, cậu đẹp trai thật."
	resUnpunct := qa.ValidateSegment(srcUnpunct, tgtUnpunct, "zh", "vi")
	if !resUnpunct.Passed {
		t.Fatalf("expected clause-final question tag '不' followed by new clause in %q not to trigger negation inversion, got: %v (err: %v)", srcUnpunct, resUnpunct.Violations, resUnpunct.Err)
	}

	// Case 3: Other dialectal/colloquial tag-question markers with clause-level boundary in unpunctuated streams:
	// e.g. "...行不行...", "...好不好...", "...对不对..." or clause-boundary "...好不我走了", "...对不你说呢"
	for _, tagSrc := range []struct {
		src string
		tgt string
	}{
		{"你明天来不我们一起去", "Ngày mai cậu tới chứ, chúng ta cùng đi nhé."},
		{"这件衣服好看不行的话就换", "Bộ đồ này đẹp nhỉ, được thì lấy không thì đổi."},
		{"这道菜好吃不对吧", "Món này ngon đúng không."},
	} {
		r := qa.ValidateSegment(tagSrc.src, tagSrc.tgt, "zh", "vi")
		if !r.Passed {
			t.Errorf("expected tag question %q to pass QA, got: %v", tagSrc.src, r.Violations)
		}
	}

	// Negative Controls (Fail-closed verification):
	// Genuine negation using 不 must REMAIN fail-closed when translated affirmatively!
	for _, negSrc := range []struct {
		src string
		tgt string
	}{
		{"他不喜欢你你长得不帅", "Anh ấy thích cậu, cậu đẹp trai."},
		{"我不会去参加这个会议", "Tôi sẽ tham gia cuộc họp này."},
		{"不能打开窗户", "Có thể mở cửa sổ."},
		{"不仅如此他还不吃肉", "Hơn nữa anh ấy thích ăn thịt."},
		{"他不是学生", "Anh ấy là học sinh."},
		{"走不走", "Đi thôi."},
	} {
		r := qa.ValidateSegment(negSrc.src, negSrc.tgt, "zh", "vi")
		if r.Passed {
			t.Errorf("expected genuine negation %q translated affirmatively as %q to FAIL QA, but passed", negSrc.src, negSrc.tgt)
		}
		if !errors.Is(r.Err, domain.ErrNegationInverted) {
			t.Errorf("expected ErrNegationInverted for %q, got: %v", negSrc.src, r.Err)
		}
	}
}

// TestMeaningFirstQAGate_ProtectedASCIITokenPassThrough is the production
// regression for asset 7674828203650994041 region-036: the OCR brand fragment
// "CH" must pass through verbatim. A hallucinated negation sentence over that
// brand must stay fail-closed.
func TestMeaningFirstQAGate_ProtectedASCIITokenPassThrough(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// Verbatim pass-through of the protected-ASCII brand fragment.
	rt := qa.ValidateSegment("CH", "CH", "zh", "vi")
	if !rt.Passed {
		t.Fatalf("expected bare protected-ASCII brand 'CH' -> 'CH' to pass QA, got: %v (err: %v)", rt.Violations, rt.Err)
	}
	if rt.Confidence < 0.9 {
		t.Fatalf("expected high confidence for verbatim CH pass-through, got %v", rt.Confidence)
	}

	// Hallucinated negation over the brand must remain fail-closed.
	rtNeg := qa.ValidateSegment("CH", "Tôi không biết", "zh", "vi")
	if rtNeg.Passed {
		t.Fatalf("expected hallucinated negation translation of 'CH' to FAIL QA")
	}
	if !errors.Is(rtNeg.Err, domain.ErrNegationInverted) {
		t.Fatalf("expected ErrNegationInverted for hallucinated negation over 'CH', got: %v", rtNeg.Err)
	}
}

// TestMeaningFirstQAGate_Seg21ProductionCandidate is the production regression
// for asset 7679392272936389915 segment 21: the adverbial "一直" must not be
// treated as a quantity, and the clause-final tag-particle "不" must not be
// treated as semantic negation, so the Vietnamese candidate passes.
func TestMeaningFirstQAGate_Seg21ProductionCandidate(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()
	src := "我 也 是 对 了 哥 们 我 咋 咋 一 直 朝 咱 这 儿 看 呢 他 应 该 是 喜 欢 你 不 你 长 得 真 帅"
	tgt := "Phải rồi anh bạn, sao nó cứ nhìn về chúng ta mãi vậy, chắc nó thích anh đúng không, anh trông thật bảnh"
	res := qa.ValidateSegment(src, tgt, "zh", "vi")
	if !res.Passed {
		t.Fatalf("expected segment 21 candidate to pass QA, got: %v (err: %v)", res.Violations, res.Err)
	}
	if res.Confidence < 0.9 {
		t.Fatalf("expected high confidence for seg21 candidate, got %v", res.Confidence)
	}
}

// TestMeaningFirstQAGate_TraditionalNegationMarker is the production regression
// for asset 7674828203650994041 visual-track region-103/170: the source uses
// traditional "沒" (U+6C92), not simplified "没" (U+6CA1). The QA gate's zh
// negator lexicon must recognize both forms, otherwise correctly-negative
// translations are wrongly rejected (affirmative -> negative) and genuine
// inversions slip through.
func TestMeaningFirstQAGate_TraditionalNegationMarker(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. Correctly-negative translation of traditional 沒 source must PASS.
	res := qa.ValidateSegment(
		"沒吃完可以这样密封起来",
		"Có thể đóng kín như thế này nếu chưa ăn hết",
		"zh", "vi",
	)
	if !res.Passed {
		t.Fatalf("expected correctly-negative translation of traditional 沒 source to pass QA, got violations: %v, err: %v", res.Violations, res.Err)
	}
	if res.Confidence < 0.9 {
		t.Fatalf("expected high confidence for traditional 沒 candidate, got %v", res.Confidence)
	}
	if !res.NegationPolarity {
		t.Fatalf("expected source to be detected as negation for traditional 沒, got affirmative")
	}

	// 2. Simplified 沒 variant must behave identically.
	resSimp := qa.ValidateSegment(
		"没吃完可以这样密封起来",
		"Có thể đóng kín như thế này nếu chưa ăn hết",
		"zh", "vi",
	)
	if !resSimp.Passed {
		t.Fatalf("expected correctly-negative translation of simplified 没 source to pass QA, got violations: %v, err: %v", resSimp.Violations, resSimp.Err)
	}

	// 3. Genuine inversion of traditional 沒 source (target drops negation) must
	// remain fail-closed.
	resInvert := qa.ValidateSegment(
		"沒吃完可以这样密封起来",
		"Có thể đóng kín như thế này",
		"zh", "vi",
	)
	if resInvert.Passed {
		t.Fatalf("expected genuine negation-drop inversion of traditional 沒 source to FAIL QA, but it passed")
	}
	if !errors.Is(resInvert.Err, domain.ErrNegationInverted) {
		t.Fatalf("expected ErrNegationInverted for genuine inversion, got: %v", resInvert.Err)
	}
}

func TestMeaningFirstQAGate_AmbiguousNegationCompounds(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	// 1. Genuinely negative compounds translated with target negators (e.g. không đủ, không bằng, chưa chắc)
	// must PASS rather than being rejected as affirmative -> negative inversions.
	passCases := []struct {
		name    string
		src     string
		tgt     string
		srcLang string
		tgtLang string
	}{
		{
			name:    "不足 with Vietnamese negator (không đủ)",
			src:     "他的经验不足以胜任这份工作",
			tgt:     "Kinh nghiệm của anh ấy không đủ để đảm nhận công việc này",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "不足 with affirmative lexicalization (thiếu)",
			src:     "他的经验不足以胜任这份工作",
			tgt:     "Kinh nghiệm của anh ấy thiếu để đảm nhận công việc này",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "不如 with Vietnamese negator (không bằng)",
			src:     "做这个不如做那个",
			tgt:     "Làm cái này không bằng làm cái kia",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "不如 with affirmative lexicalization (kém hơn)",
			src:     "做这个不如做那个",
			tgt:     "Làm cái này kém hơn làm cái kia",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "不一定 with Vietnamese negator (chưa chắc)",
			src:     "明天不一定会下雪",
			tgt:     "Ngày mai chưa chắc sẽ có tuyết rơi",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "不一定 with Vietnamese negator (không hẳn)",
			src:     "明天不一定会下雪",
			tgt:     "Ngày mai không hẳn sẽ có tuyết rơi",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "不一定 with affirmative lexicalization (có lẽ)",
			src:     "明天不一定会下雪",
			tgt:     "Ngày mai có lẽ sẽ có tuyết rơi",
			srcLang: "zh",
			tgtLang: "vi",
		},
	}

	for _, tc := range passCases {
		t.Run(tc.name, func(t *testing.T) {
			res := qa.ValidateSegment(tc.src, tc.tgt, tc.srcLang, tc.tgtLang)
			if !res.Passed {
				t.Fatalf("expected %q -> %q to pass QA, but failed: violations=%v err=%v",
					tc.src, tc.tgt, res.Violations, res.Err)
			}
		})
	}

	// 2. Ordinary affirmative text with no negation, translated with a negator,
	// MUST FAIL with ErrNegationInverted (hard requirement: live inversion detection).
	failInversions := []struct {
		name    string
		src     string
		tgt     string
		srcLang string
		tgtLang string
	}{
		{
			name:    "Ordinary affirmative translated with negator",
			src:     "这首歌的旋律非常动听",
			tgt:     "Giai điệu bài hát này không hay",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "True polarity-neutral compound (非常) translated with negator",
			src:     "今天的天气非常暖和",
			tgt:     "Hôm nay thời tiết không ấm áp",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "True polarity-neutral compound (无聊) translated with negator",
			src:     "这部电影真无聊",
			tgt:     "Bộ phim này không nhạt nhẽo",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Ambiguous compound with genuine standalone negator dropped",
			src:     "方案不足，但请不要放弃",
			tgt:     "Phương án thiếu, nhưng hãy từ bỏ",
			srcLang: "zh",
			tgtLang: "vi",
		},
	}

	for _, tc := range failInversions {
		t.Run(tc.name, func(t *testing.T) {
			res := qa.ValidateSegment(tc.src, tc.tgt, tc.srcLang, tc.tgtLang)
			if res.Passed {
				t.Fatalf("expected genuine inversion %q -> %q to FAIL QA, but it passed", tc.src, tc.tgt)
			}
			if !errors.Is(res.Err, domain.ErrNegationInverted) {
				t.Fatalf("expected ErrNegationInverted, got: %v", res.Err)
			}
		})
	}
}

func TestTranslationService_FlaggedFallback_RefusesOnRouterFailClosed(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	jobID := "job-fail-closed-test"
	seedTranslationTestRun(t, db, ctx, assetID, runID, jobID)

	// Candidate 1 (primary, fake_llm_translator) trips the QA gate by corrupting semantic facts.
	p1, ok := reg.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	fake1 := p1.(*provider.FakeTranslationProvider)
	fake1.CustomTranslations["今天天气很好。"] = "Hôm nay thời tiết rất tệ."

	// Candidate 2 (fallback, fake_local_translator_fallback) fails with a fail-closed provenance error.
	p2, ok := reg.Get("fake_local_translator_fallback")
	if !ok {
		t.Fatal("fake_local_translator_fallback not found")
	}
	fake2 := p2.(*provider.FakeTranslationProvider)
	fake2.InjectError = domain.ErrInconsistentProvenance

	_, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          jobID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1000},
		},
	})
	if err == nil {
		t.Fatalf("expected fail-closed router error to abort execution, but got nil err and published variant")
	}
	if !errors.Is(err, domain.ErrInconsistentProvenance) {
		t.Fatalf("expected ErrInconsistentProvenance to be preserved, got: %v", err)
	}
}
func seedTranslationTestRun(t *testing.T, db *storage.DB, ctx context.Context, assetID, runID, jobID string) {
	t.Helper()
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "fake-sha256",
		ByteSize:            1024,
		CreatedAt:           time.Now().UTC(),
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		CreatedAt:      time.Now().UTC(),
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             uuid.NewString(),
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: "fake-transcript-sha256",
		CreatedAt:      time.Now().UTC(),
	})
}

func TestTranslationService_FlaggedFallback_PublishesWhenAllLanesTripQA(t *testing.T) {
	db, casStore, router, reg := setupTranslationTestEnv(t)
	svc := service.NewTranslationService(db, casStore)
	svc.ConfigureRouter(router)

	ctx := context.Background()
	runID := uuid.NewString()
	assetID := uuid.NewString()
	jobID := "job-flagged-test"
	seedTranslationTestRun(t, db, ctx, assetID, runID, jobID)

	// Both candidate 1 and candidate 2 trip the QA gate with semantic fact corruption.
	p1, ok := reg.Get("fake_llm_translator")
	if !ok {
		t.Fatal("fake_llm_translator not found")
	}
	fake1 := p1.(*provider.FakeTranslationProvider)
	fake1.CustomTranslations["今天天气很好。"] = "Hôm nay thời tiết rất tệ."

	p2, ok := reg.Get("fake_local_translator_fallback")
	if !ok {
		t.Fatal("fake_local_translator_fallback not found")
	}
	fake2 := p2.(*provider.FakeTranslationProvider)
	fake2.CustomTranslations["今天天气很好。"] = "Hôm nay thời tiết rất tệ."

	variant, err := svc.Translate(ctx, domain.TranslationJobInput{
		RunID:          runID,
		AssetID:        assetID,
		JobID:          jobID,
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", StartMs: 0, EndMs: 1000},
		},
	})
	if err != nil {
		t.Fatalf("expected flagged fallback to publish best flagged candidate, got err: %v", err)
	}
	if variant == nil || len(variant.Segments) != 1 {
		t.Fatalf("expected published variant with 1 segment, got: %v", variant)
	}
	if variant.Segments[0].PassedQAGate {
		t.Fatalf("expected segment to stay flagged, got PassedQAGate=true")
	}
}
