package cubepath_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/cubepath"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/fakeapi"
)

const testToken = "tok-secret-0123456789"

func newClient(t *testing.T, h http.Handler, obs func(api, result string)) (*cubepath.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := cubepath.New(cubepath.Options{BaseURL: srv.URL, Token: testToken, UserAgent: "cubepath-k8s-exporter/test", Observe: obs})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestValidateBaseURL(t *testing.T) {
	good := []string{"https://api.cubepath.com", "https://api.cubepath.com/", "http://localhost:8000", "http://127.0.0.1:1234", "https://example.org/base"}
	for _, u := range good {
		if _, err := cubepath.ValidateBaseURL(u); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
	bad := []string{"http://api.cubepath.com", "http://10.0.0.1", "ftp://x", "https://", "api.cubepath.com", "https://user:pw@api.cubepath.com", "https://a.com?x=1", "http://localhost.evil.com"}
	for _, u := range bad {
		if _, err := cubepath.ValidateBaseURL(u); err == nil {
			t.Errorf("%s: expected an error", u)
		}
	}
}

func TestNewRequiresToken(t *testing.T) {
	if _, err := cubepath.New(cubepath.Options{BaseURL: "https://api.cubepath.com"}); err == nil {
		t.Fatal("expected an error without a token")
	}
}

func TestHeadersSent(t *testing.T) {
	var got http.Header
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":{"kubernetesClusters":[]}}`))
	}), nil)
	if _, _, err := c.ClusterRefs(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer "+testToken || got.Get("User-Agent") != "cubepath-k8s-exporter/test" || got.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", got)
	}
}

func TestGraphQLDataWithErrors(t *testing.T) {
	body := `{"data":{"c0":{"uuid":"u1","name":"n1","health":null,"nodes":[]}},
	 "errors":[{"message":"x","path":["c0","health"],"extensions":{"code":"METRICS_UNAVAILABLE"}}]}`
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), nil)
	res, err := c.ClusterDetails(context.Background(), []string{"u1"})
	if err != nil {
		t.Fatalf("data with errors must not be an error: %v", err)
	}
	if got := res.ByUUID["u1"]; got == nil || got.UUID != "u1" || got.Health != nil {
		t.Fatalf("clusters: %+v", res.ByUUID)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code() != cubepath.CodeMetricsUnavail || res.Errors[0].PathRoot() != "c0" {
		t.Fatalf("errors: %+v", res.Errors)
	}
}

func TestClusterRefsDecode(t *testing.T) {
	var sent map[string]any
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte(`{"data":{"kubernetesClusters":[{"uuid":"u1","name":"n1"},{"uuid":"u2","name":"n2"}]}}`))
	}), nil)
	refs, _, err := c.ClusterRefs(context.Background(), "42")
	if err != nil || len(refs) != 2 || refs[1].UUID != "u2" {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	q, _ := sent["query"].(string)
	if strings.Contains(q, "health") || strings.Contains(q, "nodes") {
		t.Fatalf("the list query must not read health or nodes (they are priced per cluster): %s", q)
	}
	if vars, _ := sent["variables"].(map[string]any); vars["projectId"] != "42" {
		t.Fatalf("variables: %v", sent["variables"])
	}
}

func TestClusterQueryShapeAndLimits(t *testing.T) {
	if cubepath.MaxClustersPerRequest != 15 || cubepath.PointsPerCluster != 240 {
		t.Fatalf("chunk %d, price %d", cubepath.MaxClustersPerRequest, cubepath.PointsPerCluster)
	}
	if cubepath.MaxClustersPerRequest*cubepath.PointsPerCluster > cubepath.BudgetPoints {
		t.Fatal("a full chunk must fit the point budget")
	}
	var ids []string
	for i := 0; i < cubepath.MaxClustersPerRequest; i++ {
		ids = append(ids, fmt.Sprintf("11111111-1111-1111-1111-1111111111%02d", i))
	}
	q, vars, err := cubepath.BuildClusterQuery(ids)
	if err != nil || len(vars) != 15 || strings.Contains(q, ids[0]) || strings.Count(q, "kubernetesCluster(uuid:") != 15 {
		t.Fatalf("err=%v vars=%d\n%s", err, len(vars), q)
	}
	if _, _, err := cubepath.BuildClusterQuery(append(ids, "x")); err == nil {
		t.Fatal("16 clusters must be refused")
	}
	if _, _, err := cubepath.BuildClusterQuery([]string{"a) { x"}); err == nil {
		t.Fatal("an unsafe uuid must be refused")
	}
}

func TestClustersErrorCodes(t *testing.T) {
	for _, code := range []string{"UNAUTHENTICATED", "FORBIDDEN", "NOT_FOUND", "BAD_USER_INPUT", "QUERY_TOO_COMPLEX", "INTERNAL_ERROR", "SOMETHING_NEW"} {
		t.Run(code, func(t *testing.T) {
			body := fmt.Sprintf(`{"data":null,"errors":[{"message":"m","path":["kubernetesClusters"],"extensions":{"code":%q}}]}`, code)
			c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), nil)
			_, errs, err := c.ClusterRefs(context.Background(), "")
			var qe *cubepath.QueryError
			if !errors.As(err, &qe) {
				t.Fatalf("expected a QueryError, got %v", err)
			}
			want := cubepath.NormalizeCode(code)
			if qe.Code != want || cubepath.ErrorCode(err) != want || len(errs) != 1 {
				t.Fatalf("code %q, want %q", qe.Code, want)
			}
			if code == "UNAUTHENTICATED" && !errors.Is(err, cubepath.ErrUnauthenticated) {
				t.Fatal("UNAUTHENTICATED must match ErrUnauthenticated")
			}
		})
	}
}

func TestMetricsUnavailableKeepsData(t *testing.T) {
	// METRICS_UNAVAILABLE is added next to the data: health null, nodes still listed.
	body := `{"data":{"c0":{"uuid":"u","name":"n","health":null,"nodes":[{"name":"a","ready":null,"vps":{"id":"1","name":"a"}}]}},
	"errors":[{"message":"m","path":["c0","nodes"],"extensions":{"code":"METRICS_UNAVAILABLE"}}]}`
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), nil)
	res, err := c.ClusterDetails(context.Background(), []string{"u"})
	cl := res.ByUUID["u"]
	if err != nil || cl == nil || len(cl.Nodes) != 1 || cl.Nodes[0].Ready != nil || len(res.Errors) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestClusterDetailsRootErrors(t *testing.T) {
	for _, code := range []string{"QUERY_TOO_COMPLEX", "UNAUTHENTICATED"} {
		body := fmt.Sprintf(`{"data":null,"errors":[{"message":"m","extensions":{"code":%q}}]}`, code)
		c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), nil)
		_, err := c.ClusterDetails(context.Background(), []string{"u"})
		if cubepath.ErrorCode(err) != code {
			t.Errorf("%s: err=%v", code, err)
		}
	}
}

func TestHTTPStatuses(t *testing.T) {
	cases := []struct {
		status int
		check  func(error) bool
		code   string
	}{
		{401, func(e error) bool { return errors.Is(e, cubepath.ErrUnauthenticated) }, "UNAUTHENTICATED"},
		{403, func(e error) bool { var h *cubepath.HTTPError; return errors.As(e, &h) && h.Status == 403 }, "HTTP_403"},
		{500, func(e error) bool { var h *cubepath.HTTPError; return errors.As(e, &h) && h.Status == 500 }, "HTTP_5XX"},
		{418, func(e error) bool { var h *cubepath.HTTPError; return errors.As(e, &h) }, "HTTP_OTHER"},
	}
	for _, tc := range cases {
		c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }), nil)
		_, _, err := c.ClusterRefs(context.Background(), "")
		if err == nil || !tc.check(err) || cubepath.ErrorCode(err) != tc.code {
			t.Errorf("status %d: err=%v code=%s", tc.status, err, cubepath.ErrorCode(err))
		}
	}
}

func TestRateLimitRetryAfter(t *testing.T) {
	var mu sync.Mutex
	results := map[string]int{}
	obs := func(api, result string) { mu.Lock(); results[api+"/"+result]++; mu.Unlock() }
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}), obs)
	_, _, err := c.ClusterRefs(context.Background(), "")
	var rl *cubepath.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second || cubepath.ErrorCode(err) != "RATE_LIMITED" {
		t.Fatalf("err=%v", err)
	}
	if results["graphql/rate_limited"] != 1 {
		t.Fatalf("results: %v", results)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":      60 * time.Second,
		"abc":   60 * time.Second,
		"5":     5 * time.Second,
		"0":     time.Second,
		"99999": 10 * time.Minute,
		now.Add(30 * time.Second).Format(time.RFC1123): 30 * time.Second,
	}
	for in, want := range cases {
		if got := cubepath.ParseRetryAfter(in, now); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestResponseTooLarge(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"pad":"` + strings.Repeat("x", cubepath.MaxResponseBytes) + `"}`))
	}), nil)
	_, _, err := c.ClusterRefs(context.Background(), "")
	if !errors.Is(err, cubepath.ErrResponseTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecodeError(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`not json`)) }), nil)
	if _, _, err := c.ClusterRefs(context.Background(), ""); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused
	c, err := cubepath.New(cubepath.Options{BaseURL: url, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ClusterRefs(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err=%v", err)
	}
	if cubepath.ErrorCode(err) != cubepath.CodeNetwork {
		t.Fatalf("code=%s", cubepath.ErrorCode(err))
	}
}

func TestCrossOriginRedirectRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect target must not be called (auth header %q)", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/graphql", http.StatusTemporaryRedirect)
	}), nil)
	if _, _, err := c.ClusterRefs(context.Background(), ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestProjectIDValidated(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("must not be called") }), nil)
	if _, _, err := c.ClusterRefs(context.Background(), `1) { x`); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRESTDecode(t *testing.T) {
	var paths []string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/kubernetes/" {
			_, _ = w.Write([]byte(`[{"uuid":"u1","project_id":7,"name":"a","status":"active","version":"v1.31.5","ha_control_plane":true,"location":{"id":1,"location_name":"bcn-1"}}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"uuid":"p","name":"default","plan":{"name":"gp","ram":4096,"cpu":2,"storage":50},"desired_nodes":2,"min_nodes":1,"max_nodes":4,"auto_scale":true,"nodes":[{"vps_id":5,"vps_name":"x","k8s_status":"ready","node_name":null}]}]`))
	}), nil)
	list, err := c.RESTClusters(context.Background())
	if err != nil || len(list) != 1 || list[0].Location.LocationName != "bcn-1" || *list[0].HAControlPlane != true || list[0].ProjectID != 7 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	pools, err := c.RESTPools(context.Background(), "u1")
	if err != nil || len(pools) != 1 || *pools[0].Plan.RAM != 4096 || pools[0].Nodes[0].NodeName != nil {
		t.Fatalf("pools=%+v err=%v", pools, err)
	}
	if strings.Join(paths, ",") != "/kubernetes/,/kubernetes/u1/node-pools/" {
		t.Fatalf("paths: %v", paths)
	}
	if _, err := c.RESTPools(context.Background(), "../etc"); err == nil {
		t.Fatal("expected an error for an unsafe uuid")
	}
}

func TestChunkSize(t *testing.T) {
	if cubepath.ChunkSize != 15 {
		t.Fatalf("chunk size %d, want 15", cubepath.ChunkSize)
	}
	if cubepath.PointsPerVM != 1200 {
		t.Fatalf("points per VM %d, want 1200", cubepath.PointsPerVM)
	}
	cases := []struct{ alias, budget, per, want int }{
		{15, 24000, 1200, 15}, // the alias limit binds (the budget would allow 20)
		{15, 12000, 1200, 10}, // the budget binds
		{15, 100, 1200, 1},    // never below one
		{15, 24000, 0, 15},
	}
	for _, c := range cases {
		if got := cubepath.ChunkSizeFor(c.alias, c.budget, c.per); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}

func TestChunks(t *testing.T) {
	cases := map[int][]int{0: nil, 1: {1}, 15: {15}, 16: {15, 1}, 40: {15, 15, 10}, 200: {15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 5}}
	for n, want := range cases {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprint(1000 + i)
		}
		chunks := cubepath.Chunks(ids)
		var got []int
		total := 0
		for _, c := range chunks {
			got = append(got, len(c))
			total += len(c)
			if len(c)*cubepath.PointsPerVM > cubepath.BudgetPoints || len(c) > cubepath.MaxAliasesPerRequest {
				t.Errorf("n=%d: chunk of %d breaks a limit", n, len(c))
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) || total != n {
			t.Errorf("n=%d: got %v want %v", n, got, want)
		}
	}
}

func TestBuildVMQuery(t *testing.T) {
	q, vars, err := cubepath.BuildVMQuery([]string{"1001", "1002"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "1001") || strings.Contains(q, "1002") {
		t.Fatal("ids must travel as variables, not in the document")
	}
	if vars["i0"] != "1001" || vars["i1"] != "1002" || len(vars) != 2 {
		t.Fatalf("vars: %v", vars)
	}
	for _, want := range []string{"v0: vps(id: $i0)", "v1: vps(id: $i1)", "metrics(range: H1)", "networkBreakdown(range: H1)", "$i0: ID!, $i1: ID!"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q", want)
		}
	}
	for _, bad := range [][]string{{}, {"abc"}, {"1) { x"}, {""}, {"-1"}, make([]string, 16)} {
		if _, _, err := cubepath.BuildVMQuery(bad); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
}

func TestVMsAgainstFakeLimits(t *testing.T) {
	fake := fakeapi.New("tok")
	ids := make([]string, 15)
	for i := range ids {
		ids[i] = fmt.Sprint(5000 + i)
		fake.VMs[ids[i]] = `{"id":"` + ids[i] + `","metrics":{"step":30,"series":[]},"networkBreakdown":{"step":30,"series":[]}}`
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c, _ := cubepath.New(cubepath.Options{BaseURL: srv.URL, Token: "tok"})
	res, err := c.VMs(context.Background(), ids)
	if err != nil || len(res.ByID) != 15 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := c.VMs(context.Background(), append(ids, "5999")); err == nil {
		t.Fatal("16 ids must be refused before sending")
	}
}

func TestVMsForbiddenAndUnauthenticated(t *testing.T) {
	fake := fakeapi.New("tok")
	fake.VMs["1"] = `{"id":"1","metrics":{"step":30,"series":[]},"networkBreakdown":{"step":30,"series":[]}}`
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c, _ := cubepath.New(cubepath.Options{BaseURL: srv.URL, Token: "tok"})
	fake.SetForbidVPS(true)
	res, err := c.VMs(context.Background(), []string{"1", "2"})
	if err != nil || len(res.Forbidden) != 2 || len(res.ByID) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	bad, _ := cubepath.New(cubepath.Options{BaseURL: srv.URL, Token: "other"})
	if _, err := bad.VMs(context.Background(), []string{"1"}); !errors.Is(err, cubepath.ErrUnauthenticated) {
		t.Fatalf("err=%v", err)
	}
}

func TestSecretRedaction(t *testing.T) {
	s := cubepath.Secret("super-secret")
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q", s, s, s, s, s), fmt.Sprintf("%v", struct{ T cubepath.Secret }{s})} {
		if strings.Contains(out, "super-secret") {
			t.Fatalf("leak: %s", out)
		}
	}
	if s.Reveal() != "super-secret" {
		t.Fatal("Reveal must return the value")
	}
}
