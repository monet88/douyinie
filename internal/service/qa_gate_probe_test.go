package service

import (
	"testing"
)

func TestQAGateProbe_ReproducedCandidates(t *testing.T) {
	g := NewMeaningFirstQAGate()
	cases := []struct {
		name, src, tgt, srcLang, tgtLang string
	}{
		{"CH_echo_instruction", "CH", "Cắt gọn tất cả các số, chữ số, lượng, và cực tính phủ định một cách nghiêm ngặt. Không bỏ qua bất kỳ số nào.", "zh", "vi"},
		{"seg21_reproduced", "我 也 是 对 了 哥 们 我 咋 咋 一 直 朝 咱 这 儿 看 呢 他 应 该 是 喜 欢 你 不 你 长 得 真 帅", "Tôi cũng là đúng rồi anh em, tôi cứ nhìn về đây mãi mà, anh ấy chắc là thích cậu, cậu thật sự rất帅", "zh", "vi"},
		{"seg21_old_bad_candidate", "我 也 是 对 了 哥 们 我 咋 咋 一 直 朝 咱 这 儿 看 呢 他 应 该 是 喜 欢 你 不 你 长 得 真 帅", "Anh ấy chắc là thích cậu đúng không? Cậu thật sự đẹp trai", "zh", "vi"},
	}
	for _, c := range cases {
		res := g.ValidateSegment(c.src, c.tgt, c.srcLang, c.tgtLang)
		t.Logf("%s => Passed=%v Confidence=%.2f Err=%v", c.name, res.Passed, res.Confidence, res.Err)
	}
}
