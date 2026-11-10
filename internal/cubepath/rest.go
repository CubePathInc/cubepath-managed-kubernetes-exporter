package cubepath

import (
	"context"
	"errors"
	"net/url"
	"regexp"
)

// RESTCluster is one entry of GET /kubernetes/.
type RESTCluster struct {
	UUID           string `json:"uuid"`
	ProjectID      int64  `json:"project_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Version        string `json:"version"`
	HAControlPlane *bool  `json:"ha_control_plane"`
	Location       *struct {
		LocationName string `json:"location_name"`
	} `json:"location"`
}

// RESTPlan is the plan of a node pool (ram in MB, storage in GB).
type RESTPlan struct {
	Name    string `json:"name"`
	RAM     *int64 `json:"ram"`
	CPU     *int64 `json:"cpu"`
	Storage *int64 `json:"storage"`
}

// RESTPoolNode is a worker as listed by a node pool.
type RESTPoolNode struct {
	VPSID     int64   `json:"vps_id"`
	VPSName   string  `json:"vps_name"`
	VPSStatus string  `json:"vps_status"`
	K8sStatus string  `json:"k8s_status"`
	NodeName  *string `json:"node_name"`
}

// RESTPool is one entry of GET /kubernetes/{uuid}/node-pools/.
type RESTPool struct {
	UUID         string         `json:"uuid"`
	Name         string         `json:"name"`
	Plan         *RESTPlan      `json:"plan"`
	DesiredNodes *int64         `json:"desired_nodes"`
	MinNodes     *int64         `json:"min_nodes"`
	MaxNodes     *int64         `json:"max_nodes"`
	AutoScale    *bool          `json:"auto_scale"`
	Nodes        []RESTPoolNode `json:"nodes"`
}

var uuidRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ValidClusterUUID reports whether s is safe to place in a URL path.
func ValidClusterUUID(s string) bool { return uuidRe.MatchString(s) }

// RESTClusters lists the clusters of the organization.
func (c *Client) RESTClusters(ctx context.Context) ([]RESTCluster, error) {
	var out []RESTCluster
	if err := c.do(ctx, APIREST, "GET", "/kubernetes/", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RESTPools lists the node pools of one cluster.
func (c *Client) RESTPools(ctx context.Context, clusterUUID string) ([]RESTPool, error) {
	if !ValidClusterUUID(clusterUUID) {
		return nil, errors.New("invalid cluster uuid")
	}
	var out []RESTPool
	if err := c.do(ctx, APIREST, "GET", "/kubernetes/"+url.PathEscape(clusterUUID)+"/node-pools/", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
