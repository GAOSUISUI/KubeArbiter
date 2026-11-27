package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/aiops"
)

type incidentPage struct {
	NextCursor *string          `json:"next_cursor"`
	Items      []aiops.Incident `json:"items"`
}

func listIncidents(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Incidents == nil {
			writeError(c, errUnavailable())
			return
		}
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "limit", Reason: err.Error()}))
			return
		}
		filter := aiops.Filter{
			Status:    c.Query("status"),
			Type:      c.Query("type"),
			Severity:  c.Query("severity"),
			Namespace: c.Query("namespace"),
			Target:    c.Query("target"),
		}
		items, err := d.Incidents.ListIncidents(c.Request.Context(), filter)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		start := 0
		if raw := c.Query("cursor"); raw != "" {
			id, ok := decodeCursor(raw)
			if !ok {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标格式非法"}))
				return
			}
			idx := -1
			for i := range items {
				if items[i].ID == id {
					idx = i
					break
				}
			}
			if idx < 0 {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标已失效"}))
				return
			}
			start = idx + 1
		}

		rest := items[start:]
		page := incidentPage{Items: []aiops.Incident{}}
		if len(rest) > limit {
			page.Items = append(page.Items, rest[:limit]...)
			next := encodeCursor(rest[limit-1].ID)
			page.NextCursor = &next
		} else {
			page.Items = append(page.Items, rest...)
		}
		writeOK(c, page)
	}
}

func getIncident(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Incidents == nil {
			writeError(c, errUnavailable())
			return
		}
		detail, ok, err := d.Incidents.GetIncident(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}
		writeOK(c, detail)
	}
}
