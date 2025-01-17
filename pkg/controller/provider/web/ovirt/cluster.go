package ovirt

import (
	"errors"
	"github.com/gin-gonic/gin"
	libmodel "github.com/konveyor/controller/pkg/inventory/model"
	api "github.com/konveyor/forklift-controller/pkg/apis/forklift/v1beta1"
	model "github.com/konveyor/forklift-controller/pkg/controller/provider/model/ovirt"
	"github.com/konveyor/forklift-controller/pkg/controller/provider/web/base"
	"net/http"
)

//
// Routes.
const (
	ClusterParam      = "cluster"
	ClusterCollection = "clusters"
	ClustersRoot      = ProviderRoot + "/" + ClusterCollection
	ClusterRoot       = ClustersRoot + "/:" + ClusterParam
)

//
// Cluster handler.
type ClusterHandler struct {
	Handler
	// Selected cluster.
	cluster *model.Cluster
}

//
// Add routes to the `gin` router.
func (h *ClusterHandler) AddRoutes(e *gin.Engine) {
	e.GET(ClustersRoot, h.List)
	e.GET(ClustersRoot+"/", h.List)
	e.GET(ClusterRoot, h.Get)
}

//
// List resources in a REST collection.
// A GET onn the collection that includes the `X-Watch`
// header will negotiate an upgrade of the connection
// to a websocket and push watch events.
func (h ClusterHandler) List(ctx *gin.Context) {
	status := h.Prepare(ctx)
	if status != http.StatusOK {
		ctx.Status(status)
		return
	}
	if h.WatchRequest {
		h.watch(ctx)
		return
	}
	db := h.Collector.DB()
	list := []model.Cluster{}
	err := db.List(&list, h.ListOptions(ctx))
	if err != nil {
		log.Trace(
			err,
			"url",
			ctx.Request.URL)
		ctx.Status(http.StatusInternalServerError)
		return
	}
	content := []interface{}{}
	for _, m := range list {
		r := &Cluster{}
		r.With(&m)
		r.Link(h.Provider)
		content = append(content, r.Content(h.Detail))
	}

	ctx.JSON(http.StatusOK, content)
}

//
// Get a specific REST resource.
func (h ClusterHandler) Get(ctx *gin.Context) {
	status := h.Prepare(ctx)
	if status != http.StatusOK {
		ctx.Status(status)
		return
	}
	m := &model.Cluster{
		Base: model.Base{
			ID: ctx.Param(ClusterParam),
		},
	}
	db := h.Collector.DB()
	err := db.Get(m)
	if errors.Is(err, model.NotFound) {
		ctx.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		log.Trace(
			err,
			"url",
			ctx.Request.URL)
		ctx.Status(http.StatusInternalServerError)
		return
	}
	r := &Cluster{}
	r.With(m)
	r.Link(h.Provider)
	content := r.Content(true)

	ctx.JSON(http.StatusOK, content)
}

//
// Watch.
func (h ClusterHandler) watch(ctx *gin.Context) {
	db := h.Collector.DB()
	err := h.Watch(
		ctx,
		db,
		&model.Cluster{},
		func(in libmodel.Model) (r interface{}) {
			m := in.(*model.Cluster)
			cluster := &Cluster{}
			cluster.With(m)
			cluster.Link(h.Provider)
			r = cluster
			return
		})
	if err != nil {
		log.Trace(
			err,
			"url",
			ctx.Request.URL)
		ctx.Status(http.StatusInternalServerError)
	}
}

//
// REST Resource.
type Cluster struct {
	Resource
	DataCenter    string `json:"dataCenter"`
	HaReservation bool   `json:"haReservation"`
	KsmEnabled    bool   `json:"ksmEnabled"`
}

//
// Build the resource using the model.
func (r *Cluster) With(m *model.Cluster) {
	r.Resource.With(&m.Base)
	r.DataCenter = m.DataCenter
	r.HaReservation = m.HaReservation
	r.KsmEnabled = m.KsmEnabled
}

//
// Build self link (URI).
func (r *Cluster) Link(p *api.Provider) {
	r.SelfLink = base.Link(
		ClusterRoot,
		base.Params{
			base.ProviderParam: string(p.UID),
			ClusterParam:       r.ID,
		})
}

//
// As content.
func (r *Cluster) Content(detail bool) interface{} {
	if !detail {
		return r.Resource
	}

	return r
}
