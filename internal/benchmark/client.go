package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

// RuntimeHostClient is an external HTTP client for RuntimeHost's public versioned API (Seam 1).
// Invariant: The benchmark runner sequences only existing public/versioned endpoints;
// it does not call private service internals or create a benchmark-only product API.
type RuntimeHostClient struct {
	baseURL    string
	httpClient *http.Client
}

// StageRouteOptions carries the non-secret routing context that must accompany
// benchmark requests whose production behavior varies by execution profile.
// Credential values remain behind RuntimeHost credential references or runtime
// environment resolution; only safe credential reference IDs are transported.
type StageRouteOptions struct {
	ExecutionProfile      domain.ExecutionProfile
	AuthorizedCredentials []string
	ConsentGranted        bool
}

func applyStageRouteOptions(payload map[string]any, opts StageRouteOptions, includeConsent bool) {
	if opts.ExecutionProfile != "" {
		payload["execution_profile"] = opts.ExecutionProfile
	}
	if len(opts.AuthorizedCredentials) > 0 {
		payload["authorized_credentials"] = append([]string(nil), opts.AuthorizedCredentials...)
	}
	if includeConsent {
		payload["consent_granted"] = opts.ConsentGranted
	}
}

// HTTPError captures the actual HTTP status code and response body from RuntimeHost.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// NewRuntimeHostClient constructs a new public Seam 1 client.
func NewRuntimeHostClient(baseURL string, httpClient *http.Client) *RuntimeHostClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &RuntimeHostClient{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

func (c *RuntimeHostClient) doJSON(ctx context.Context, method, path string, reqBody any, respOut any) (int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("execute request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode, Body: string(respBytes)}
	}

	if respOut != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, respOut); err != nil {
			return resp.StatusCode, fmt.Errorf("unmarshal response (%s): %w", string(respBytes), err)
		}
	}

	return resp.StatusCode, nil
}

// IngestAsset calls POST /api/v1/assets/ingest.
func (c *RuntimeHostClient) IngestAsset(ctx context.Context, filePath string, declaredBy string, termsAccepted bool) (*domain.SourceAsset, error) {
	payload := map[string]any{
		"file_path": filePath,
		"attestation": map[string]any{
			"declared_by":    declaredBy,
			"terms_accepted": termsAccepted,
		},
	}
	var res struct {
		Asset domain.SourceAsset `json:"asset"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/assets/ingest", payload, &res); err != nil {
		return nil, fmt.Errorf("ingest asset: %w", err)
	}
	return &res.Asset, nil
}

// AcquireSource calls POST /api/v1/sources/acquire.
func (c *RuntimeHostClient) AcquireSource(ctx context.Context, req service.AcquireRequest) (*service.AcquireResult, error) {
	var res service.AcquireResult
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/sources/acquire", req, &res); err != nil {
		return nil, fmt.Errorf("acquire source: %w", err)
	}
	return &res, nil
}

// ProbeSource calls POST /api/v1/sources/probe.
func (c *RuntimeHostClient) ProbeSource(ctx context.Context, req service.AcquireRequest) (*domain.SourceDescriptor, error) {
	var res struct {
		Descriptor *domain.SourceDescriptor `json:"descriptor"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/sources/probe", req, &res); err != nil {
		return nil, fmt.Errorf("probe source: %w", err)
	}
	return res.Descriptor, nil
}

// GetAsset calls GET /api/v1/assets/{id}.
func (c *RuntimeHostClient) GetAsset(ctx context.Context, assetID string) (*domain.SourceAsset, error) {
	var res struct {
		Asset domain.SourceAsset `json:"asset"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v1/assets/"+assetID, nil, &res); err != nil {
		return nil, fmt.Errorf("get asset: %w", err)
	}
	return &res.Asset, nil
}

// GetAssetPreflight calls GET /api/v1/assets/{id}/preflight.
func (c *RuntimeHostClient) GetAssetPreflight(ctx context.Context, assetID string) (*domain.PreflightReport, error) {
	var res struct {
		PreflightReport domain.PreflightReport `json:"preflight_report"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/assets/%s/preflight", assetID), nil, &res); err != nil {
		return nil, fmt.Errorf("get asset preflight: %w", err)
	}
	return &res.PreflightReport, nil
}

// CreateJob calls POST /api/v1/jobs.
func (c *RuntimeHostClient) CreateJob(ctx context.Context, sourceAssetID, targetLang string) (*domain.LocalizationJob, error) {
	payload := map[string]string{
		"source_asset_id": sourceAssetID,
		"target_language": targetLang,
	}
	var res struct {
		Job domain.LocalizationJob `json:"job"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/jobs", payload, &res); err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	return &res.Job, nil
}

// GetJob calls GET /api/v1/jobs/{id}.
func (c *RuntimeHostClient) GetJob(ctx context.Context, jobID string) (*domain.LocalizationJob, error) {
	var res struct {
		Job domain.LocalizationJob `json:"job"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v1/jobs/"+jobID, nil, &res); err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return &res.Job, nil
}

// CreateRun calls POST /api/v1/jobs/{id}/runs.
func (c *RuntimeHostClient) CreateRun(ctx context.Context, jobID, configSnapshotJSON string) (*domain.LocalizationRun, error) {
	if configSnapshotJSON == "" {
		configSnapshotJSON = "{}"
	}
	payload := map[string]string{
		"config_snapshot_json": configSnapshotJSON,
	}
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/jobs/%s/runs", jobID), payload, &res); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	return &res.Run, nil
}

// GetRun calls GET /api/v1/runs/{id}.
func (c *RuntimeHostClient) GetRun(ctx context.Context, runID string) (*domain.LocalizationRun, error) {
	var res struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, "/api/v1/runs/"+runID, nil, &res); err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return &res.Run, nil
}

// SaveAudioRolePlan calls POST /api/v1/assets/{id}/audio-role-plan.
func (c *RuntimeHostClient) SaveAudioRolePlan(ctx context.Context, assetID string, segments []domain.AudioSegment) (*domain.AudioRolePlan, error) {
	payload := map[string]any{
		"segments": segments,
	}
	var res struct {
		Plan domain.AudioRolePlan `json:"plan"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/audio-role-plan", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("save audio role plan: %w", err)
	}
	return &res.Plan, nil
}

// RunSpeechUnderstand calls POST /api/v1/assets/{id}/speech-understand.
func (c *RuntimeHostClient) RunSpeechUnderstand(ctx context.Context, assetID, runID string) (*domain.TranscriptArtifact, error) {
	payload := map[string]any{"run_id": runID}
	var res struct {
		Transcript domain.TranscriptArtifact `json:"transcript_artifact"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/speech-understand", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("speech understand: %w", err)
	}
	return &res.Transcript, nil
}

// RunTranslation calls POST /api/v1/assets/{id}/translate.
func (c *RuntimeHostClient) RunTranslation(ctx context.Context, assetID, runID, jobID, targetLang string, segments []domain.TranslationInputSegment, opts StageRouteOptions) (*domain.TranslationVariant, error) {
	payload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": targetLang,
		"segments":        segments,
	}
	applyStageRouteOptions(payload, opts, true)
	var res struct {
		TranslationVariant domain.TranslationVariant `json:"translation_variant"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/translate", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("translate: %w", err)
	}
	return &res.TranslationVariant, nil
}

// RunDubScript calls POST /api/v1/assets/{id}/dub-script.
func (c *RuntimeHostClient) RunDubScript(ctx context.Context, assetID, runID, jobID, targetLang, transCAS string, segments []domain.TranslationInputSegment, opts StageRouteOptions) (*domain.DubScriptVariant, error) {
	payload := map[string]any{
		"run_id":                  runID,
		"job_id":                  jobID,
		"target_language":         targetLang,
		"translation_variant_cas": transCAS,
		"segments":                segments,
	}
	applyStageRouteOptions(payload, opts, false)
	var res struct {
		DubScriptVariant domain.DubScriptVariant `json:"dub_script_variant"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/dub-script", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("dub script: %w", err)
	}
	return &res.DubScriptVariant, nil
}

// AssignVoices calls POST /api/v1/assets/{id}/voice-assignment.
func (c *RuntimeHostClient) AssignVoices(ctx context.Context, assetID, runID, jobID, targetLang string, opts StageRouteOptions) (*domain.VoiceAssignment, error) {
	payload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": targetLang,
	}
	applyStageRouteOptions(payload, opts, false)
	var res struct {
		VoiceAssignment domain.VoiceAssignment `json:"voice_assignment"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/voice-assignment", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("assign voices: %w", err)
	}
	return &res.VoiceAssignment, nil
}

// RunDubSynthesize calls POST /api/v1/assets/{id}/dub-synthesize.
func (c *RuntimeHostClient) RunDubSynthesize(ctx context.Context, assetID, runID, jobID, targetLang, voiceAssignCAS, dubScriptCAS string, opts StageRouteOptions) (*domain.DubSegmentsVariant, error) {
	payload := map[string]any{
		"run_id":               runID,
		"job_id":               jobID,
		"target_language":      targetLang,
		"voice_assignment_cas": voiceAssignCAS,
		"dub_script_cas":       dubScriptCAS,
	}
	applyStageRouteOptions(payload, opts, false)
	var res struct {
		DubSegmentsVariant domain.DubSegmentsVariant `json:"dub_segments_variant"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/dub-synthesize", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("dub synthesize: %w", err)
	}
	return &res.DubSegmentsVariant, nil
}

// SeparateStems calls POST /api/v1/assets/{id}/separate-stems.
func (c *RuntimeHostClient) SeparateStems(ctx context.Context, assetID, runID string, opts StageRouteOptions) (*domain.AudioStemArtifacts, error) {
	payload := map[string]any{"run_id": runID}
	applyStageRouteOptions(payload, opts, false)
	var res struct {
		Stems domain.AudioStemArtifacts `json:"audio_stems"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/separate-stems", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("separate stems: %w", err)
	}
	return &res.Stems, nil
}

// RunAudioMix calls POST /api/v1/assets/{id}/audio-mix.
func (c *RuntimeHostClient) RunAudioMix(ctx context.Context, assetID, runID, jobID, targetLang, dubSegmentsCAS, audioStemsCAS string, opts StageRouteOptions) (*domain.DubMixArtifact, error) {
	payload := map[string]any{
		"run_id":           runID,
		"job_id":           jobID,
		"target_language":  targetLang,
		"dub_segments_cas": dubSegmentsCAS,
		"audio_stems_cas":  audioStemsCAS,
	}
	applyStageRouteOptions(payload, opts, false)
	var res struct {
		DubMix domain.DubMixArtifact `json:"dub_mix"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/audio-mix", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("audio mix: %w", err)
	}
	return &res.DubMix, nil
}

// DetectText calls POST /api/v1/assets/{id}/detect-text.
func (c *RuntimeHostClient) DetectText(ctx context.Context, assetID, runID string, opts StageRouteOptions) (*domain.TextRegionPlan, error) {
	payload := map[string]any{"run_id": runID}
	applyStageRouteOptions(payload, opts, false)
	var res struct {
		TextRegionPlan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/detect-text", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("detect text: %w", err)
	}
	return &res.TextRegionPlan, nil
}

// LocalizeVisualTrack calls POST /api/v1/assets/{id}/visual-track.
func (c *RuntimeHostClient) LocalizeVisualTrack(ctx context.Context, assetID, runID, jobID, targetLang, textRegionCAS, transCAS string, opts StageRouteOptions) (*domain.LocalizedVisualTrack, *domain.LocalizedSubtitleTrack, error) {
	payload := map[string]any{
		"run_id":                  runID,
		"job_id":                  jobID,
		"target_language":         targetLang,
		"text_region_plan_cas":    textRegionCAS,
		"translation_variant_cas": transCAS,
	}
	applyStageRouteOptions(payload, opts, true)
	var res struct {
		VisualTrack   domain.LocalizedVisualTrack   `json:"localized_visual_track"`
		SubtitleTrack domain.LocalizedSubtitleTrack `json:"localized_subtitle_track"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/visual-track", assetID), payload, &res); err != nil {
		return nil, nil, fmt.Errorf("visual track: %w", err)
	}
	return &res.VisualTrack, &res.SubtitleTrack, nil
}

// FreezeRenderPlan calls POST /api/v1/assets/{id}/render-plan.
func (c *RuntimeHostClient) FreezeRenderPlan(ctx context.Context, assetID, runID, jobID, targetLang, dubMixCAS, subPlanCAS string) (*domain.RenderPlan, error) {
	payload := map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   targetLang,
		"dub_mix_cas":       dubMixCAS,
		"subtitle_plan_cas": subPlanCAS,
	}
	var res struct {
		RenderPlan domain.RenderPlan `json:"render_plan"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/render-plan", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("freeze render plan: %w", err)
	}
	return &res.RenderPlan, nil
}

// RenderFinal calls POST /api/v1/assets/{id}/render/final.
func (c *RuntimeHostClient) RenderFinal(ctx context.Context, assetID, runID, jobID, targetLang, renderPlanCAS string) (*domain.FinalRenderArtifact, error) {
	payload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": targetLang,
		"plan_cas":        renderPlanCAS,
	}
	var res struct {
		FinalRender domain.FinalRenderArtifact `json:"final_render"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/api/v1/assets/%s/render/final", assetID), payload, &res); err != nil {
		return nil, fmt.Errorf("render final: %w", err)
	}
	return &res.FinalRender, nil
}

// PostQualityResult calls POST /api/v1/quality-results.
func (c *RuntimeHostClient) PostQualityResult(ctx context.Context, qr domain.QualityResult) (*domain.QualityResult, error) {
	var res struct {
		QualityResult domain.QualityResult `json:"quality_result"`
	}
	if _, err := c.doJSON(ctx, http.MethodPost, "/api/v1/quality-results", qr, &res); err != nil {
		return nil, fmt.Errorf("post quality result: %w", err)
	}
	return &res.QualityResult, nil
}

// GetRunQualityResults calls GET /api/v1/runs/{id}/quality-results.
func (c *RuntimeHostClient) GetRunQualityResults(ctx context.Context, runID string) ([]domain.QualityResult, error) {
	var res struct {
		QualityResults []domain.QualityResult `json:"quality_results"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/runs/%s/quality-results", runID), nil, &res); err != nil {
		return nil, fmt.Errorf("get run quality results: %w", err)
	}
	return res.QualityResults, nil
}

// GetRunReviewItems calls GET /api/v1/runs/{id}/review-items.
func (c *RuntimeHostClient) GetRunReviewItems(ctx context.Context, runID string, includeResolved bool) ([]domain.ReviewItem, error) {
	path := fmt.Sprintf("/api/v1/runs/%s/review-items", runID)
	if includeResolved {
		path += "?include_resolved=true"
	}
	var res struct {
		Items []domain.ReviewItem `json:"items"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, fmt.Errorf("get run review items: %w", err)
	}
	return res.Items, nil
}

// ListAttempts calls GET /api/v1/routing/attempts?run_id=...&stage=...
func (c *RuntimeHostClient) ListAttempts(ctx context.Context, runID, stage string) ([]domain.ProviderAttempt, error) {
	params := url.Values{}
	if runID != "" {
		params.Set("run_id", runID)
	}
	if stage != "" {
		params.Set("stage", stage)
	}
	path := "/api/v1/routing/attempts"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var res struct {
		Attempts []domain.ProviderAttempt `json:"attempts"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	return res.Attempts, nil
}

// ListDecisions calls GET /api/v1/routing/decisions?run_id=...&stage=...
func (c *RuntimeHostClient) ListDecisions(ctx context.Context, runID, stage string) ([]domain.SelectionDecision, error) {
	params := url.Values{}
	if runID != "" {
		params.Set("run_id", runID)
	}
	if stage != "" {
		params.Set("stage", stage)
	}
	path := "/api/v1/routing/decisions"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var res struct {
		Decisions []domain.SelectionDecision `json:"decisions"`
	}
	if _, err := c.doJSON(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	return res.Decisions, nil
}
