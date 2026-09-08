package service_test

import (
	"testing"

	"github.com/monet88/douyinie/internal/service"
)

func TestMeaningFirstQAGate_SmokePatterns(t *testing.T) {
	qa := service.NewMeaningFirstQAGate()

	tests := []struct {
		name    string
		source  string
		target  string
		srcLang string
		tgtLang string
	}{
		{
			name:    "Pattern 1a: Spaced 告 别 in 7651915111247413428_vi",
			source:  "今 天 分 享 口 播 剪 辑 中 的 七 种 画 面 处 理 方 式 让 你 的 视 频 告 别 平 淡 过 渡 一 下 开 始 吧",
			target:  "Hôm nay tôi chia sẻ 7 cách xử lý hình ảnh giúp video của bạn tạm biệt sự nhạt nhòa, chuyển cảnh một chút rồi bắt đầu nhé.",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Pattern 1b: 是不是 and 无聊 in 7576536040861715775_en",
			source:  "你剪辑的口播是不是很死板人物一直输出很无聊我的答案是开头打上关键帧",
			target:  "Are your talking-head edits rigid, with the subject talking boringly? My answer is to set a keyframe at the start",
			srcLang: "zh",
			tgtLang: "en",
		},
		{
			name:    "Pattern 1c: rhetorical question in 7651818380458739385_vi",
			source:  "是怕主播偷你家大米吗",
			target:  "Chẳng lẽ sợ streamer trộm gạo nhà bạn à?",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Pattern 1d: Spaced Japanese negation in 7649028199477509605_en",
			source:  "どんな 鳥 たどてもつきます 小 鳥 の 小 さな 空 風 が 吹 く 渡 り 鳥 は 飛 ぶ に も あ り ま せ ん 小 鳥 の 小 さな 空 期 待",
			target:  "No matter what bird arrives, it is the little bird's small sky. The wind blows. Migratory birds have nowhere to fly. The little bird's small sky.",
			srcLang: "zh",
			tgtLang: "en",
		},
		{
			name:    "Pattern 1e: 不小心 in visual_track of 7674828203650994041_vi",
			source:  "蕃茄酱不小心滴到可以这样擦",
			target:  "Tương cà lỡ dính vào có thể lau như thế này.",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Pattern 2a: SOY in visual_track of 7674828203650994041_vi",
			source:  "SOY",
			target:  "Đậu nành",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Pattern 2b: SoAWATER in visual_track of 7674828203650994041_vi",
			source:  "SoAWATER",
			target:  "Nước soda",
			srcLang: "zh",
			tgtLang: "vi",
		},
		{
			name:    "Pattern 3: Indefinite article mot in 7679392272936389915_vi segment 25",
			source:  "不管是九十公斤还是一百公斤根本就没有区别都只是令人讨厌的猪精罢了才瘦了十公斤而已根本不够我还要继续瘦瘦成大美",
			target:  "dù là 90 kg hay 100 kg cũng không có gì khác biệt, đều chỉ là một con lợn tinh đáng ghét mà thôi, mới giảm có 10 kg mà thôi căn bản không đủ, tôi còn phải tiếp tục giảm để trở thành một đại mỹ nhân",
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
		})
	}
}
