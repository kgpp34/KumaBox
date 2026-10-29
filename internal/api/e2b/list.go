package e2b

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kumabox/kumabox/internal/types"
)

// listCursor uses the last creation time and ID rather than an offset so that
// deleting or inserting other sandboxes does not shift the following page.
type listCursor struct {
	CreatedAt time.Time       `json:"created_at"`
	ID        types.SandboxID `json:"id"`
	Order     string          `json:"order"`
}

type sandboxQuery struct {
	metadata     map[string]string
	states       []string
	template     string
	startedAfter time.Time
	order        string
	limit        int
	cursor       *listCursor
}

func parseSandboxQuery(values url.Values) (sandboxQuery, error) {
	query := sandboxQuery{order: "desc", limit: 100, template: values.Get("template")}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return query, errors.New("limit must be between 1 and 100")
		}
		query.limit = limit
	}
	if raw := values.Get("order"); raw != "" {
		if raw != "asc" && raw != "desc" {
			return query, errors.New("order must be asc or desc")
		}
		query.order = raw
	}
	for _, raw := range values["state"] {
		for state := range strings.SplitSeq(raw, ",") {
			if state != "running" && state != "paused" {
				return query, errors.New("state must be running or paused")
			}
			query.states = append(query.states, state)
		}
	}
	if raw := values.Get("startedAfter"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return query, errors.New("startedAfter must be an RFC3339 timestamp")
		}
		query.startedAfter = parsed
	}
	// Official SDKs percent-encode each key/value, then encode the resulting
	// query string again as one metadata parameter. Decode both inner layers.
	metadata, err := url.ParseQuery(values.Get("metadata"))
	if err != nil {
		return query, errors.New("invalid metadata filter")
	}
	query.metadata = make(map[string]string, len(metadata))
	for key, values := range metadata {
		if len(values) != 1 {
			return query, errors.New("metadata filter keys must be unique")
		}
		decodedKey, keyErr := url.PathUnescape(key)
		decodedValue, valueErr := url.PathUnescape(values[0])
		if keyErr != nil || valueErr != nil {
			return query, errors.New("invalid metadata filter encoding")
		}
		if _, exists := query.metadata[decodedKey]; exists {
			return query, errors.New("decoded metadata filter keys must be unique")
		}
		query.metadata[decodedKey] = decodedValue
	}
	if raw := values.Get("nextToken"); raw != "" {
		data, err := base64.RawURLEncoding.DecodeString(raw)
		var cursor listCursor
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Order != query.order {
			return query, errors.New("invalid nextToken for this order")
		}
		if _, err := types.ParseSandboxID(cursor.ID.String()); err != nil {
			return query, errors.New("invalid nextToken sandbox ID")
		}
		query.cursor = &cursor
	}
	return query, nil
}

func compareSandbox(record types.Sandbox, createdAt time.Time, id types.SandboxID) int {
	if compared := record.CreatedAt.Compare(createdAt); compared != 0 {
		return compared
	}
	return cmp.Compare(record.ID, id)
}

func (q sandboxQuery) matches(record types.Sandbox, policy lease) bool {
	state := policy.state(record)
	if state == "" || len(q.states) != 0 && !slices.Contains(q.states, state) {
		return false
	}
	if q.template != "" && q.template != policy.template(record) {
		return false
	}
	if !q.startedAfter.IsZero() && record.CreatedAt.Before(q.startedAfter) {
		return false
	}
	for key, value := range q.metadata {
		if actual, exists := policy.Metadata[key]; !exists || actual != value {
			return false
		}
	}
	if q.cursor != nil {
		compared := compareSandbox(record, q.cursor.CreatedAt, q.cursor.ID)
		if q.order == "asc" && compared <= 0 || q.order == "desc" && compared >= 0 {
			return false
		}
	}
	return true
}

// listSandboxes presents only resources with an E2B lease. Native sandboxes do
// not acquire E2B ownership simply because they share the same application.
func (h *handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	query, err := parseSandboxQuery(r.URL.Query())
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	records, err := h.sandboxes.List(r.Context(), true)
	if err != nil {
		serviceFailure(w, err)
		return
	}
	records = slices.Clone(records)
	slices.SortFunc(records, func(a, b types.Sandbox) int {
		compared := compareSandbox(a, b.CreatedAt, b.ID)
		if query.order == "desc" {
			return -compared
		}
		return compared
	})
	result := make([]map[string]any, 0, query.limit)
	var last types.Sandbox
	for _, record := range records {
		policy, exists, err := h.leases.get(record.ID)
		if err != nil {
			serviceFailure(w, err)
			return
		}
		if !exists || !query.matches(record, policy) {
			continue
		}
		if len(result) == query.limit {
			data, err := json.Marshal(listCursor{CreatedAt: last.CreatedAt, ID: last.ID, Order: query.order})
			if err != nil {
				serviceFailure(w, err)
				return
			}
			w.Header().Set("X-Next-Token", base64.RawURLEncoding.EncodeToString(data))
			break
		}
		result = append(result, sandboxInfo(record, policy))
		last = record
	}
	jsonResponse(w, http.StatusOK, result)
}

func (p lease) template(record types.Sandbox) string {
	if p.TemplateID != "" {
		return p.TemplateID
	}
	// Leases created by an older adapter did not retain the image alias.
	return record.ImageDigest.String()
}

func (p lease) state(record types.Sandbox) string {
	switch {
	case record.State == types.SandboxStateRunning:
		return "running"
	case record.State == types.SandboxStateStopped && p.PausedSnapshot != "":
		return "paused"
	default:
		return ""
	}
}

// sandboxInfo contains public control metadata. Command environment defaults
// and the per-sandbox access token are deliberately absent from list results.
func sandboxInfo(record types.Sandbox, policy lease) map[string]any {
	endAt := policy.ExpiresAt
	if endAt.IsZero() {
		endAt = record.UpdatedAt
	}
	metadata := policy.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	return map[string]any{
		"sandboxID": record.ID.String(), "templateID": policy.template(record),
		"clientID": "", "envdVersion": "0.1.0",
		"startedAt": record.CreatedAt, "endAt": endAt,
		"state": policy.state(record), "cpuCount": record.Config.CPUs,
		"memoryMB": record.Config.Memory / (1 << 20), "diskSizeMB": record.Config.Storage / (1 << 20),
		"metadata": metadata,
	}
}
