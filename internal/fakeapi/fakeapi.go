// Package fakeapi is a stand-in for the CubePath API, used only by tests and by the
// fakeapid smoke tool. It mimics the documented limits (aliases, tokens, point budget).
package fakeapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Documented limits of POST /graphql, enforced here like the real API does.
const (
	maxAliases   = 15
	maxTokens    = 2000
	budgetPoints = 24000
	pointsPerVPS = 1200
	// health and nodes are charged once per cluster, 120 points each, and nodePools 12.
	pointsHealthOrNodes = 120
	pointsNodePools     = 12
	rateRetryHint       = "7"
)

var (
	aliasRe    = regexp.MustCompile(`\bv\d+\s*:\s*vps\s*\(`)
	clusterRe  = regexp.MustCompile(`\bc\d+\s*:\s*kubernetesCluster\s*\(`)
	templateRe = regexp.MustCompile(`@\{(-?\d+)\}`)
	tokenRe    = regexp.MustCompile(`\.\.\.|[A-Za-z_][A-Za-z0-9_]*|-?[0-9]+(?:\.[0-9]+)?|[{}()\[\]:!$=@|&]|"[^"]*"`)
	aliasNumRe = regexp.MustCompile(`^v(\d+)$`)
	// newFieldRe matches the fields added in schema v1.7.
	newFieldRe = regexp.MustCompile(`\b(nodePools|haControlPlane|location|status)\b`)
	healthRe   = regexp.MustCompile(`\bhealth\s*[{(]`)
	nodesRe    = regexp.MustCompile(`\bnodes\s*[{(]`)
	poolsRe    = regexp.MustCompile(`\bnodePools\s*[{(]`)
)

// fieldPoints is the price of the priced fields of one cluster in query.
func fieldPoints(query string) int {
	n := 0
	if healthRe.MatchString(query) {
		n += pointsHealthOrNodes
	}
	if nodesRe.MatchString(query) {
		n += pointsHealthOrNodes
	}
	if poolsRe.MatchString(query) {
		n += pointsNodePools
	}
	return n
}

// Server is the fake API. Set the exported fields before serving; they are read-only afterwards,
// except through the Set methods.
type Server struct {
	// Token is the only accepted bearer token.
	Token string
	// Now returns the unix time that "@{offset}" placeholders of the fixtures resolve against.
	Now func() int64

	// ClustersBody is the full JSON response to the kubernetesClusters query.
	ClustersBody string
	// VMs maps a server id to the JSON of its vps object.
	VMs map[string]string

	mu            sync.Mutex
	forbidVPS     bool
	graphqlStatus int
	retryAfter    string
	legacySchema  bool
	detailStatus  int
	detailRetry   string
	vmStatus      int
	vmRetry       string
	clusterFaults map[string]string
	clusterCalls  int
	detailCalls   int
	vmCalls       int
	detailQueries []string
	detailIDs     [][]string
	vmQueries     []string
	vmIDs         [][]string
}

// New returns an empty fake that accepts token.
func New(token string) *Server {
	return &Server{Token: token, Now: func() int64 { return time.Now().Unix() }, VMs: map[string]string{}}
}

// LoadFixture reads graphql_clusters.json and vps.json from dir.
func LoadFixture(dir, token string) (*Server, error) {
	s := New(token)
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		return string(b), err
	}
	var err error
	if s.ClustersBody, err = read("graphql_clusters.json"); err != nil {
		return nil, err
	}
	for name, dst := range map[string]*map[string]string{"vps.json": &s.VMs} {
		raw, err := read(name)
		if err != nil {
			return nil, err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(templateRe.ReplaceAllString(raw, "0")), &m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out := map[string]string{}
		// Keep the placeholders: split the raw document per top-level key textually.
		for k := range m {
			out[k] = ""
		}
		*dst = splitTop(raw, out)
	}
	return s, nil
}

// splitTop re-reads the raw object (with placeholders) and returns each top-level value as text.
// It is a small scanner: the fixtures are flat objects of objects or arrays.
func splitTop(raw string, keys map[string]string) map[string]string {
	out := map[string]string{}
	for k := range keys {
		loc := regexp.MustCompile(`"` + regexp.QuoteMeta(k) + `"\s*:\s*[\[{]`).FindStringIndex(raw)
		j := loc[1] - 1
		open := raw[j]
		closeCh := byte('}')
		if open == '[' {
			closeCh = ']'
		}
		depth, inStr := 0, false
		end := j
		for ; end < len(raw); end++ {
			c := raw[end]
			if inStr {
				if c == '\\' {
					end++
				} else if c == '"' {
					inStr = false
				}
				continue
			}
			if c == '"' {
				inStr = true
			} else if c == open {
				depth++
			} else if c == closeCh {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		out[k] = raw[j : end+1]
	}
	return out
}

// SetForbidVPS makes every vps(id:) answer FORBIDDEN, like a token without vps:read.
func (s *Server) SetForbidVPS(v bool) { s.mu.Lock(); s.forbidVPS = v; s.mu.Unlock() }

// SetGraphQLStatus makes POST /graphql answer a bare HTTP status (0 restores normal answers).
func (s *Server) SetGraphQLStatus(status int, retryAfter string) {
	s.mu.Lock()
	s.graphqlStatus, s.retryAfter = status, retryAfter
	s.mu.Unlock()
}

// SetClusterDetailStatus makes the per cluster detail queries answer a bare HTTP status.
func (s *Server) SetClusterDetailStatus(status int, retryAfter string) {
	s.mu.Lock()
	s.detailStatus, s.detailRetry = status, retryAfter
	s.mu.Unlock()
}

// SetVMStatus makes the server queries answer a bare HTTP status (0 restores normal answers).
func (s *Server) SetVMStatus(status int, retryAfter string) {
	s.mu.Lock()
	s.vmStatus, s.vmRetry = status, retryAfter
	s.mu.Unlock()
}

// SetClusterFault makes the detail query answer null and an error with code for one cluster
// ("" removes the fault), like a resolver that failed for it.
func (s *Server) SetClusterFault(uuid, code string) {
	s.mu.Lock()
	if s.clusterFaults == nil {
		s.clusterFaults = map[string]string{}
	}
	if code == "" {
		delete(s.clusterFaults, uuid)
	} else {
		s.clusterFaults[uuid] = code
	}
	s.mu.Unlock()
}

// SetLegacySchema makes the fake behave like an API older than schema v1.7: a cluster detail
// document that asks for status, version, haControlPlane, location, nodePools or a node's pool
// is rejected as a whole with BAD_USER_INPUT.
func (s *Server) SetLegacySchema(v bool) { s.mu.Lock(); s.legacySchema = v; s.mu.Unlock() }

// SetClustersBody replaces the response of the kubernetesClusters query.
func (s *Server) SetClustersBody(body string) { s.mu.Lock(); s.ClustersBody = body; s.mu.Unlock() }

// Calls is a copy of the request counters.
type Calls struct {
	// Clusters counts the cluster list queries, ClusterDetails the per cluster detail queries.
	Clusters, ClusterDetails, VMs int
	ClusterQueries                []string
	ClusterIDs                    [][]string
	VMQueries                     []string
	VMIDs                         [][]string
}

// Calls returns what the fake has served.
func (s *Server) Calls() Calls {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Calls{s.clusterCalls, s.detailCalls, s.vmCalls,
		append([]string(nil), s.detailQueries...), append([][]string(nil), s.detailIDs...),
		append([]string(nil), s.vmQueries...), append([][]string(nil), s.vmIDs...)}
}

func (s *Server) render(raw string) string {
	now := s.Now()
	return templateRe.ReplaceAllStringFunc(raw, func(m string) string {
		off, _ := strconv.ParseInt(templateRe.FindStringSubmatch(m)[1], 10, 64)
		return strconv.FormatInt(now+off, 10)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.Token
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		s.graphql(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func gqlErr(code, msg string, path ...any) string {
	b, _ := json.Marshal(map[string]any{"message": msg, "path": path, "extensions": map[string]string{"code": code}})
	return string(b)
}

func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	status, retry := s.graphqlStatus, s.retryAfter
	s.mu.Unlock()
	if status != 0 {
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(status)
		return
	}
	var req struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	isDetail := clusterRe.MatchString(req.Query)
	isList := !isDetail && strings.Contains(req.Query, "kubernetesClusters")
	root := "v0"
	switch {
	case isList:
		root = "kubernetesClusters"
	case isDetail:
		root = "c0"
	}
	if !s.authorized(r) {
		writeJSON(w, `{"data":null,"errors":[`+gqlErr("UNAUTHENTICATED", "Authentication required", root)+`]}`)
		return
	}
	tokens := len(tokenRe.FindAllString(req.Query, -1))
	aliases := len(aliasRe.FindAllString(req.Query, -1))
	clusterAliases := len(clusterRe.FindAllString(req.Query, -1))
	// Health, nodes and nodePools are charged per cluster, like the real API. The list form is
	// charged per listed cluster, the detail form per alias.
	perCluster := fieldPoints(req.Query)
	listPoints := 0
	if isList {
		listPoints = s.clusterCount() * perCluster
	}
	if tokens > maxTokens || aliases > maxAliases || clusterAliases > maxAliases ||
		aliases*pointsPerVPS > budgetPoints || clusterAliases*perCluster > budgetPoints || listPoints > budgetPoints {
		writeJSON(w, `{"data":null,"errors":[`+gqlErr("QUERY_TOO_COMPLEX", fmt.Sprintf("tokens=%d aliases=%d", tokens, aliases+clusterAliases))+`]}`)
		return
	}
	if isList {
		s.mu.Lock()
		s.clusterCalls++
		s.mu.Unlock()
		writeJSON(w, s.clusterRefs())
		return
	}
	if isDetail {
		s.mu.Lock()
		legacy := s.legacySchema
		s.mu.Unlock()
		if legacy && newFieldRe.MatchString(req.Query) {
			writeJSON(w, `{"errors":[`+gqlErr("BAD_USER_INPUT", `Cannot query field "nodePools" on type "KubernetesCluster".`)+`]}`)
			return
		}
		s.clusterDetails(w, req.Query, req.Variables, clusterAliases)
		return
	}
	s.mu.Lock()
	vmStatus, vmRetry := s.vmStatus, s.vmRetry
	s.mu.Unlock()
	if vmStatus != 0 {
		if vmRetry != "" {
			w.Header().Set("Retry-After", vmRetry)
		}
		w.WriteHeader(vmStatus)
		return
	}

	s.mu.Lock()
	s.vmCalls++
	s.vmQueries = append(s.vmQueries, req.Query)
	forbid := s.forbidVPS
	s.mu.Unlock()

	var data, errs []string
	var ids []string
	for i := 0; i < aliases; i++ {
		alias := "v" + strconv.Itoa(i)
		id := req.Variables["i"+strconv.Itoa(i)]
		ids = append(ids, id)
		vm, ok := s.VMs[id]
		switch {
		case forbid:
			data = append(data, fmt.Sprintf("%q:null", alias))
			errs = append(errs, gqlErr("FORBIDDEN", "Token lacks required scope: vps:read", alias))
		case !ok:
			data = append(data, fmt.Sprintf("%q:null", alias))
			errs = append(errs, gqlErr("NOT_FOUND", "Not found", alias))
		default:
			data = append(data, fmt.Sprintf("%q:%s", alias, vm))
		}
	}
	s.mu.Lock()
	s.vmIDs = append(s.vmIDs, ids)
	s.mu.Unlock()
	body := `{"data":{` + strings.Join(data, ",") + `}`
	if len(errs) > 0 {
		body += `,"errors":[` + strings.Join(errs, ",") + `]`
	}
	writeJSON(w, s.render(body+`}`))
}

// clusterObjects parses the rendered ClustersBody into uuid-keyed raw objects, in order.
func (s *Server) clusterObjects() (uuids []string, byUUID map[string]json.RawMessage) {
	s.mu.Lock()
	body := s.ClustersBody
	s.mu.Unlock()
	var doc struct {
		Data struct {
			Clusters []json.RawMessage `json:"kubernetesClusters"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(s.render(body)), &doc); err != nil {
		return nil, nil
	}
	byUUID = map[string]json.RawMessage{}
	for _, raw := range doc.Data.Clusters {
		var head struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(raw, &head) == nil {
			uuids = append(uuids, head.UUID)
			byUUID[head.UUID] = raw
		}
	}
	return uuids, byUUID
}

func (s *Server) clusterCount() int {
	u, _ := s.clusterObjects()
	return len(u)
}

// clusterRefs answers the cluster list query: uuid and name of every cluster.
func (s *Server) clusterRefs() string {
	uuids, by := s.clusterObjects()
	var items []string
	for _, u := range uuids {
		var head struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(by[u], &head)
		b, _ := json.Marshal(head)
		items = append(items, string(b))
	}
	return `{"data":{"kubernetesClusters":[` + strings.Join(items, ",") + `]}}`
}

func (s *Server) clusterDetails(w http.ResponseWriter, query string, vars map[string]string, aliases int) {
	s.mu.Lock()
	status, retry := s.detailStatus, s.detailRetry
	faults := map[string]string{}
	for k, v := range s.clusterFaults {
		faults[k] = v
	}
	s.mu.Unlock()
	if status != 0 {
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(status)
		return
	}
	_, by := s.clusterObjects()
	var data, errs, ids []string
	for i := 0; i < aliases; i++ {
		alias := "c" + strconv.Itoa(i)
		u := vars["u"+strconv.Itoa(i)]
		ids = append(ids, u)
		raw, ok := by[u]
		switch {
		case faults[u] != "":
			data = append(data, fmt.Sprintf("%q:null", alias))
			errs = append(errs, gqlErr(faults[u], "resolver failed", alias, "nodes"))
		case !ok:
			data = append(data, fmt.Sprintf("%q:null", alias))
			errs = append(errs, gqlErr("NOT_FOUND", "Not found", alias))
		default:
			data = append(data, fmt.Sprintf("%q:%s", alias, raw))
		}
	}
	s.mu.Lock()
	s.detailCalls++
	s.detailQueries = append(s.detailQueries, query)
	s.detailIDs = append(s.detailIDs, ids)
	s.mu.Unlock()
	body := `{"data":{` + strings.Join(data, ",") + `}`
	if len(errs) > 0 {
		body += `,"errors":[` + strings.Join(errs, ",") + `]`
	}
	writeJSON(w, body+`}`)
}
