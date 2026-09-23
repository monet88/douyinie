package domain_test

import (
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestIsPathologicalRepetitionNoise_JapaneseKanaHallucination(t *testing.T) {
	// Segment 8 from 7649028199477509605 (both draft and aligned segment):
	// loud BGM ASR loop where Whisper hallucinates Japanese bird-song kana repetition.
	seg8 := "ってるチュルチュンチュンチュンチュンチュルチュンチュルチュンチュンチュンチュンチュルチュンチュルチュンチュンチュンチュンチュルチュンマカナペンキの"
	if !domain.IsPathologicalRepetitionNoise(seg8) {
		t.Fatalf("expected segment 8 kana hallucination to be classified as noise, got false")
	}

	seg8Draft := "どんな鳥だってとどきます 小鳥の 小さな 空窓 が 吹く 澄んだ窓 は 夢におりてま せん 小鳥の 小さな 窓 晴れた空の昼のランチ時 ，小鳥が パッパ つ乗って る。 チュルチュンチュンチュン チュルチュンチュンチュン カナペンキの 小 鸟 に 窓 で パイリンソルトーチェロピョ ネーヨ 。 チュルチュンチュンチュン チュルチュンチュンチュン ，チュル 小鳥の小さに穴でパイリンソルートチェロピョネー 。 小鳥 パッパつオネムリエねでは 皆 さんさようなら 明 日 は 明 日 で どうするか"
	if !domain.IsPathologicalRepetitionNoise(seg8Draft) {
		t.Fatalf("expected segment 8 draft Japanese kana ASR noise hallucination to be classified as noise, got false")
	}

	// Segment 16 from 7649028199477509605: loud BGM ASR loop where Whisper hallucinates kana repetition
	seg16 := "梅 そればかりチュルの 小 鳥 に 声 でバイリンフルートチェロピオラヨーコソヨーコソイラサキパラフクパベダラオネムリヨそれでは 皆 さんさようなら 明 日 は 明 日 でどうするか"
	if !domain.IsPathologicalRepetitionNoise(seg16) {
		t.Fatalf("expected segment 16 Japanese kana hallucination to be classified as noise, got false")
	}

	// Negative regressions: Legitimate Chinese speech must NEVER be classified as noise
	legitimateZh := []string{
		"今天是星期一，我们去公园散步，天气真好，看到了很多小鸟。",
		"这道菜真的很好吃，做法也很简单，大家可以在家里试试看。",
		"你说的确实有道理，不过我们还是要再考虑一下成本问题。",
		"哈哈，太搞笑了！",
		"哈哈哈，真有意思！",
		"哈哈哈哈", // intentional 4x laughter
		"呵呵呵",
		"谢谢大家的支持，我们下期视频再见！",
	}
	for _, s := range legitimateZh {
		if domain.IsPathologicalRepetitionNoise(s) {
			t.Fatalf("expected legitimate Chinese speech %q to NOT be noise, got true", s)
		}
	}

	// Negative regressions: Legitimate Japanese speech & onomatopoeia must NEVER be classified as noise
	legitimateJa := []string{
		"こんにちは、元気ですか。今日は天気がとてもいいですね。",
		"本日はお忙しい中お集まりいただき、誠にありがとうございます。これより本日の会議を始めさせていただきます。",
		"夜空の星がキラキラと輝いています。",
		"心がドキドキして、とても緊張しています。",
		"本当にありがとうございます！これからもよろしくお願いします。",
		"そうですね、私もそう思います。",
		"ワクワクする楽しいイベントですね。",
	}
	for _, s := range legitimateJa {
		if domain.IsPathologicalRepetitionNoise(s) {
			t.Fatalf("expected legitimate Japanese speech %q to NOT be noise, got true", s)
		}
	}

	// Negative regressions: Intentional repeated speech, filler, and onomatopoeia
	// are SEMANTIC, never noise. Reduplicative/mimetic words and emphatic or
	// agreement repetition must survive the pathological-noise guard.
	legitimateRepeat := []string{
		"对对对，你说的对",
		"走走走，我们走",
		"叮叮当当，真好听",
		"咕噜咕噜，肚子叫了",
		"哗啦哗啦，下雨了",
		"轰隆隆，打雷了",
		"叽叽喳喳，小鸟在唱歌",
		"嗯嗯嗯，我知道了",
	}
	for _, s := range legitimateRepeat {
		if domain.IsPathologicalRepetitionNoise(s) {
			t.Fatalf("expected intentional repeated speech/onomatopoeia %q to NOT be noise, got true", s)
		}
	}
}

func TestSpeechBlock_DubEligibility_PermitsOrdinaryBGMAmbience_BlocksSingingUncertain(t *testing.T) {
	plan := &domain.AudioRolePlan{
		AssetID: "asset-straddling",
		Segments: []domain.AudioSegment{
			// Dialogue with overlapping BGM/ambience
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 1000, EndMs: 3000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 2000, EndMs: 4000, Role: domain.AudioRoleAmbienceSFX},

			// Dialogue overlapping singing/music-vocal
			{StartMs: 6000, EndMs: 10000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 7000, EndMs: 8000, Role: domain.AudioRoleSingingMusicVocal},

			// Dialogue overlapping uncertain
			{StartMs: 11000, EndMs: 15000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 12000, EndMs: 13000, Role: domain.AudioRoleUncertain},
		},
	}

	// 1. Block straddling ordinary BGM/ambience must be dub-eligible
	b1 := domain.SpeechBlock{Index: 0, StartMs: 500, EndMs: 3500, SourceText: "dialogue with bgm", SegmentType: domain.SpeechBlockTypeSpeech}
	if !domain.IsDubEligibleSpeechBlock(b1, plan) {
		t.Fatalf("expected dialogue overlapping ordinary BGM/SFX to be dub-eligible, got false")
	}

	// 2. Block overlapping singing/music-vocal must be rejected
	b2 := domain.SpeechBlock{Index: 1, StartMs: 6500, EndMs: 8500, SourceText: "dialogue with singing", SegmentType: domain.SpeechBlockTypeSpeech}
	if domain.IsDubEligibleSpeechBlock(b2, plan) {
		t.Fatalf("expected dialogue overlapping singing/music-vocal to be rejected, got true")
	}

	// 3. Block overlapping uncertain vocal must be rejected
	b3 := domain.SpeechBlock{Index: 2, StartMs: 11500, EndMs: 13500, SourceText: "dialogue with uncertain", SegmentType: domain.SpeechBlockTypeSpeech}
	if domain.IsDubEligibleSpeechBlock(b3, plan) {
		t.Fatalf("expected dialogue overlapping uncertain vocal to be rejected, got true")
	}

	// 4. CanonicalTranslationSegments includes only b1
	transcript := &domain.TranscriptArtifact{
		SpeechBlocks: []domain.SpeechBlock{b1, b2, b3},
	}
	canonical := domain.CanonicalTranslationSegments(transcript, plan)
	if len(canonical) != 1 || canonical[0].Index != 0 {
		t.Fatalf("expected exactly segment 0 to be extracted as canonical translation member, got %+v", canonical)
	}
}
