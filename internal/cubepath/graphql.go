package cubepath

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Timestamp is the GraphQL Timestamp scalar: unix seconds (UTC).
type Timestamp int64

// UnmarshalJSON accepts an integer, a float or a quoted number.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("invalid Timestamp %q", s)
	}
	*t = Timestamp(int64(f))
	return nil
}

// GQLError is one entry of the GraphQL errors array.
type GQLError struct {
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// Code returns the extensions.code of the error.
func (e GQLError) Code() string { return e.Extensions.Code }

// PathRoot returns the alias or field name at the root of the error path.
func (e GQLError) PathRoot() string {
	if len(e.Path) == 0 {
		return ""
	}
	s, _ := e.Path[0].(string)
	return s
}

// HasCode reports whether any error has the given code.
func HasCode(errs []GQLError, code string) bool {
	for _, e := range errs {
		if e.Code() == code {
			return true
		}
	}
	return false
}

type envelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []GQLError      `json:"errors"`
}

// GraphQL runs one operation. Data is decoded into out whenever the response carries any,
// even when errors are present. The returned error is only for transport, HTTP and decode failures.
func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) ([]GQLError, error) {
	body, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{query, vars})
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := c.do(ctx, APIGraphQL, "POST", "/graphql", body, &env); err != nil {
		return nil, err
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			c.record(APIGraphQL, ResultDecode)
			return env.Errors, fmt.Errorf("decoding GraphQL data: %w", err)
		}
	}
	return env.Errors, nil
}

// ClusterRefsQuery lists the clusters of the token (uuid and name only). It costs nothing of the
// request budget: the expensive fields are read afterwards, a few clusters per request.
const ClusterRefsQuery = `query ClusterRefs($projectId: ID) {
  kubernetesClusters(projectId: $projectId) {
    uuid
    name
  }
}`

// clusterFragment is what is read of each cluster. health and nodes are priced per cluster,
// 120 points each, and nodePools 12 points, so 252 points per cluster.
const clusterFragment = `fragment C on KubernetesCluster {
  uuid
  name
  status
  version
  haControlPlane
  location
  nodePools {
    uuid
    name
    plan
    planCpu
    planRamMb
    planStorageGb
    desiredNodes
    minNodes
    maxNodes
    autoScale
    nodeCount
  }
  health {
    polledAt
    up
    ready
    readyzFailed
    latencyMs
    version
    nodesTotal
    nodesReady
    nodesCordoned
    podsPending
    podsFailed
    components { name ready desired }
    metricsServer
    clientCertExpires
    serverCertExpires
    errorKind
  }
  nodes {
    name
    pool
    ready
    memoryPressure
    diskPressure
    pidPressure
    unschedulable
    kubeletVersion
    allocatable { cpuM memBytes pods }
    usage { cpuM memBytes }
    readySince
    lastSeenAt
    vps { id name }
  }
}
`

// Cluster is one entry of kubernetesClusters.
type Cluster struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	// Status, Version (the configured cluster version), HAControlPlane and Location are the
	// inventory part of cluster_info.
	Status         string  `json:"status"`
	Version        string  `json:"version"`
	HAControlPlane *bool   `json:"haControlPlane"`
	Location       *string `json:"location"`
	// NodePools is nil when the field came back null (an error on that field).
	NodePools []NodePool `json:"nodePools"`
	Health    *Health    `json:"health"`
	Nodes     []Node     `json:"nodes"`
}

// NodePool is one node pool of a cluster (RAM in MB, storage in GB). NodeCount counts the
// workers that are not removed.
type NodePool struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name"`
	Plan          string `json:"plan"`
	PlanCPU       *int64 `json:"planCpu"`
	PlanRAMMB     *int64 `json:"planRamMb"`
	PlanStorageGB *int64 `json:"planStorageGb"`
	DesiredNodes  *int64 `json:"desiredNodes"`
	MinNodes      *int64 `json:"minNodes"`
	MaxNodes      *int64 `json:"maxNodes"`
	AutoScale     *bool  `json:"autoScale"`
	NodeCount     int64  `json:"nodeCount"`
}

// Health is the latest poll of the cluster's API server.
type Health struct {
	PolledAt          Timestamp   `json:"polledAt"`
	Up                bool        `json:"up"`
	Ready             bool        `json:"ready"`
	ReadyzFailed      []string    `json:"readyzFailed"`
	LatencyMs         int64       `json:"latencyMs"`
	Version           string      `json:"version"`
	NodesTotal        int64       `json:"nodesTotal"`
	NodesReady        int64       `json:"nodesReady"`
	NodesCordoned     int64       `json:"nodesCordoned"`
	PodsPending       int64       `json:"podsPending"`
	PodsFailed        int64       `json:"podsFailed"`
	Components        []Component `json:"components"`
	MetricsServer     bool        `json:"metricsServer"`
	ClientCertExpires *Timestamp  `json:"clientCertExpires"`
	ServerCertExpires *Timestamp  `json:"serverCertExpires"`
	ErrorKind         string      `json:"errorKind"`
}

// Component is a platform component of kube-system.
type Component struct {
	Name    string `json:"name"`
	Ready   int64  `json:"ready"`
	Desired int64  `json:"desired"`
}

// Resources is a CPU and memory amount (CPU in millicores).
type Resources struct {
	CPUM     *int64   `json:"cpuM"`
	MemBytes *float64 `json:"memBytes"`
	Pods     *int64   `json:"pods"`
}

// VPSRef is the server behind a node.
type VPSRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Node is a worker node. Every health field is null when the node was not seen recently.
type Node struct {
	Name           string     `json:"name"`
	Pool           *string    `json:"pool"`
	Ready          *bool      `json:"ready"`
	MemoryPressure *bool      `json:"memoryPressure"`
	DiskPressure   *bool      `json:"diskPressure"`
	PIDPressure    *bool      `json:"pidPressure"`
	Unschedulable  *bool      `json:"unschedulable"`
	KubeletVersion *string    `json:"kubeletVersion"`
	Allocatable    *Resources `json:"allocatable"`
	Usage          *Resources `json:"usage"`
	ReadySince     *Timestamp `json:"readySince"`
	LastSeenAt     *Timestamp `json:"lastSeenAt"`
	VPS            *VPSRef    `json:"vps"`
}

var projectIDRe = regexp.MustCompile(`^[0-9]{1,18}$`)

// ClusterRef is a cluster as listed by ClusterRefsQuery.
type ClusterRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// ClusterRefs lists the clusters of the token. projectID is optional and must be numeric.
func (c *Client) ClusterRefs(ctx context.Context, projectID string) ([]ClusterRef, []GQLError, error) {
	var vars map[string]any
	if projectID != "" {
		if !projectIDRe.MatchString(projectID) {
			return nil, nil, errors.New("project id must be numeric")
		}
		vars = map[string]any{"projectId": projectID}
	}
	var data struct {
		Clusters []ClusterRef `json:"kubernetesClusters"`
	}
	errs, err := c.GraphQL(ctx, ClusterRefsQuery, vars, &data)
	if err != nil {
		return nil, errs, err
	}
	for _, e := range errs {
		if e.PathRoot() == "kubernetesClusters" && len(e.Path) == 1 {
			// The root field itself failed: scope, authentication or complexity.
			return nil, errs, &QueryError{Code: NormalizeCode(e.Code())}
		}
	}
	if data.Clusters == nil {
		code := CodeUnknown
		if len(errs) > 0 {
			code = NormalizeCode(errs[0].Code())
		}
		return nil, errs, &QueryError{Code: code}
	}
	return data.Clusters, errs, nil
}

// ClustersResult is the outcome of one ClusterDetails request.
type ClustersResult struct {
	// ByUUID holds the clusters that came back, keyed by the uuid that was asked.
	ByUUID map[string]*Cluster
	Errors []GQLError
}

// BuildClusterQuery builds one operation with an aliased kubernetesCluster(uuid:) per cluster.
// Uuids travel as variables; the document itself never contains one.
func BuildClusterQuery(uuids []string) (string, map[string]any, error) {
	if len(uuids) == 0 || len(uuids) > MaxClustersPerRequest {
		return "", nil, fmt.Errorf("a cluster query carries 1 to %d clusters, got %d", MaxClustersPerRequest, len(uuids))
	}
	vars := make(map[string]any, len(uuids))
	var defs, fields strings.Builder
	for i, u := range uuids {
		if !ValidClusterUUID(u) {
			return "", nil, errors.New("invalid cluster uuid")
		}
		name := "u" + strconv.Itoa(i)
		if i > 0 {
			defs.WriteString(", ")
		}
		defs.WriteString("$" + name + ": ID!")
		fmt.Fprintf(&fields, "  c%d: kubernetesCluster(uuid: $%s) { ...C }\n", i, name)
		vars[name] = u
	}
	return "query Clusters(" + defs.String() + ") {\n" + fields.String() + "}\n" + clusterFragment, vars, nil
}

// ClusterDetails reads status, node pools, health and nodes of up to MaxClustersPerRequest clusters in one request.
// A cluster that did not come back is absent from ByUUID; its errors are in Errors.
func (c *Client) ClusterDetails(ctx context.Context, uuids []string) (ClustersResult, error) {
	res := ClustersResult{}
	q, vars, err := BuildClusterQuery(uuids)
	if err != nil {
		return res, err
	}
	var data map[string]*Cluster
	errs, err := c.GraphQL(ctx, q, vars, &data)
	res.Errors = errs
	if err != nil {
		return res, err
	}
	for _, e := range errs {
		if e.Code() == CodeQueryTooComplex || e.Code() == CodeUnauthenticated {
			return res, &QueryError{Code: NormalizeCode(e.Code())}
		}
	}
	if len(data) == 0 && HasCode(errs, CodeBadUserInput) {
		// A validation error rejects the whole document: an API older than schema v1.7 does not
		// know status, version, haControlPlane, location, nodePools or KubernetesNode.pool.
		return res, &QueryError{Code: CodeBadUserInput, Unsupported: true}
	}
	res.ByUUID = make(map[string]*Cluster, len(uuids))
	for i, u := range uuids {
		if d := data["c"+strconv.Itoa(i)]; d != nil {
			res.ByUUID[u] = d
		}
	}
	if len(res.ByUUID) == 0 {
		code := CodeUnknown
		if len(errs) > 0 {
			code = NormalizeCode(errs[0].Code())
		}
		return res, &QueryError{Code: code}
	}
	return res, nil
}

// Series is one named series of a MetricsResult.
type Series struct {
	Name   string  `json:"name"`
	Unit   string  `json:"unit"`
	Points []Point `json:"points"`
}

// Point is one sample. A null value means no sample.
type Point struct {
	TS    Timestamp `json:"ts"`
	Value *float64  `json:"value"`
}

// MetricsResult is the GraphQL MetricsResult type.
type MetricsResult struct {
	Step   int      `json:"step"`
	Series []Series `json:"series"`
}

// VMData is the server behind a node: its series.
type VMData struct {
	ID               string         `json:"id"`
	Metrics          *MetricsResult `json:"metrics"`
	NetworkBreakdown *MetricsResult `json:"networkBreakdown"`
}

// VMsResult is the outcome of one chunk request.
type VMsResult struct {
	ByID      map[string]*VMData
	Errors    []GQLError
	Requested int
	// Forbidden lists the ids whose vps(id:) answered FORBIDDEN.
	Forbidden []string
}
