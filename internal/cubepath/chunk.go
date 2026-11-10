package cubepath

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Limits and prices documented for POST /graphql (docs/graphql/README.md of the API).
const (
	// MaxAliasesPerRequest is the alias limit of one operation.
	MaxAliasesPerRequest = 15
	// BudgetPoints is the request budget (200 series-equivalents of 120 points).
	BudgetPoints = 24000
	// PointsPerH1Series is the price of one 1h series at the 30 s step.
	PointsPerH1Series = 120
	// VMMetricsSeries is the number of series of VPS.metrics (all six metrics).
	VMMetricsSeries = 6
	// VMBreakdownSeries is the number of series of VPS.networkBreakdown.
	VMBreakdownSeries = 4
	// PointsPerHealthOrNodes is the price of health or nodes on one cluster (charged once each).
	PointsPerHealthOrNodes = 120
	// PointsPerNodePools is the price of nodePools on one cluster.
	PointsPerNodePools = 12
	// PointsPerCluster is the price of one aliased kubernetesCluster(uuid:) with health, nodes and
	// nodePools: 120 + 120 + 12. status, version, haControlPlane and location are free.
	PointsPerCluster = 2*PointsPerHealthOrNodes + PointsPerNodePools
	// PointsPerVM is the price of one aliased vps(id:) with metrics and networkBreakdown over H1.
	PointsPerVM = (VMMetricsSeries + VMBreakdownSeries) * PointsPerH1Series
)

// MaxClustersPerRequest is how many clusters one request carries: the smaller of the alias limit
// and the point budget (15 by aliases, 95 by budget, so 15 and 3780 points).
var MaxClustersPerRequest = ChunkSizeFor(MaxAliasesPerRequest, BudgetPoints, PointsPerCluster)

// ChunkSize is how many servers one request carries: the smaller of what the alias
// limit and the point budget allow (15 by aliases, 20 by budget, so 15).
var ChunkSize = ChunkSizeFor(MaxAliasesPerRequest, BudgetPoints, PointsPerVM)

// ChunkSizeFor computes the largest chunk that respects both limits (at least 1).
func ChunkSizeFor(aliasLimit, budget, perItem int) int {
	n := aliasLimit
	if perItem > 0 && budget/perItem < n {
		n = budget / perItem
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Chunks splits ids into slices of at most ChunkSize.
func Chunks(ids []string) [][]string { return ChunksOf(ids, ChunkSize) }

// ChunksOf splits ids into slices of at most size.
func ChunksOf(ids []string, size int) [][]string {
	var out [][]string
	for len(ids) > 0 {
		n := size
		if len(ids) < n {
			n = len(ids)
		}
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

var uuidRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ValidClusterUUID reports whether s is a safe cluster uuid: it only ever travels as a variable.
func ValidClusterUUID(s string) bool { return uuidRe.MatchString(s) }

var vpsIDRe = regexp.MustCompile(`^[0-9]{1,18}$`)

// ValidVPSID reports whether id is a numeric server id, the only form that is ever sent.
func ValidVPSID(id string) bool { return vpsIDRe.MatchString(id) }

// BuildVMQuery builds one operation with an aliased vps(id:) per server. Ids travel as
// variables; the document itself never contains an id.
func BuildVMQuery(ids []string) (string, map[string]any, error) {
	if len(ids) == 0 || len(ids) > MaxAliasesPerRequest {
		return "", nil, fmt.Errorf("a VM query carries 1 to %d servers, got %d", MaxAliasesPerRequest, len(ids))
	}
	vars := make(map[string]any, len(ids))
	var defs, fields strings.Builder
	for i, id := range ids {
		if !ValidVPSID(id) {
			return "", nil, errors.New("invalid server id")
		}
		name := "i" + strconv.Itoa(i)
		if i > 0 {
			defs.WriteString(", ")
		}
		defs.WriteString("$" + name + ": ID!")
		fmt.Fprintf(&fields, "  v%d: vps(id: $%s) { ...VM }\n", i, name)
		vars[name] = id
	}
	q := "query VMs(" + defs.String() + ") {\n" + fields.String() + "}\n" +
		"fragment VM on VPS {\n  id\n  metrics(range: H1) { ...R }\n  networkBreakdown(range: H1) { ...R }\n}\n" +
		"fragment R on MetricsResult {\n  step\n  series { name unit points { ts value } }\n}\n"
	return q, vars, nil
}

// VMs fetches the series of up to MaxAliasesPerRequest servers in one request.
func (c *Client) VMs(ctx context.Context, ids []string) (VMsResult, error) {
	res := VMsResult{Requested: len(ids)}
	q, vars, err := BuildVMQuery(ids)
	if err != nil {
		return res, err
	}
	var data map[string]*VMData
	errs, err := c.GraphQL(ctx, q, vars, &data)
	res.Errors = errs
	if err != nil {
		return res, err
	}
	for _, e := range errs {
		if e.Code() == CodeQueryTooComplex || e.Code() == CodeUnauthenticated {
			return res, &QueryError{Code: e.Code()}
		}
	}
	res.ByID = make(map[string]*VMData, len(ids))
	for i, id := range ids {
		if d := data["v"+strconv.Itoa(i)]; d != nil {
			res.ByID[id] = d
		}
	}
	for _, e := range errs {
		if e.Code() != CodeForbidden {
			continue
		}
		root := e.PathRoot()
		if i, err := strconv.Atoi(strings.TrimPrefix(root, "v")); err == nil && strings.HasPrefix(root, "v") && i >= 0 && i < len(ids) {
			res.Forbidden = append(res.Forbidden, ids[i])
		}
	}
	return res, nil
}
