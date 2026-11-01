package api

import (
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagedashboard"
	log "github.com/sirupsen/logrus"
)

func (s *Server) serveUsageDashboard(c *gin.Context) {
	cfg := s.getConfig()
	if s.usageDashboard == nil || cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", usagedashboard.Page)
}
func (s *Server) usageSummary(c *gin.Context) {
	if s.usageDashboard == nil {
		c.JSON(404, gin.H{"error": "usage dashboard disabled"})
		return
	}
	days, err := strconv.Atoi(c.DefaultQuery("days", "1"))
	if err != nil || days < 1 || days > 90 {
		c.JSON(400, gin.H{"error": "days must be between 1 and 90"})
		return
	}
	to := time.Now().UTC()
	summary, err := s.usageDashboard.Summary(c.Request.Context(), to.Add(-time.Duration(days)*24*time.Hour), to, c.Query("provider"), c.Query("account"))
	if err != nil {
		log.WithError(err).Warn("usage summary failed")
		c.JSON(500, gin.H{"error": "usage history unavailable"})
		return
	}
	labels := map[string]string{}
	if s.handlers != nil && s.handlers.AuthManager != nil {
		for _, a := range s.handlers.AuthManager.List() {
			if a == nil {
				continue
			}
			label := a.Label
			if label == "" {
				if kind, email := a.AccountInfo(); kind == "oauth" {
					label = email
				}
			}
			if label == "" {
				label = a.Provider + " · " + usagedashboard.AccountID(a.Provider, a.ID)[:8]
			}
			labels[usagedashboard.AccountID(a.Provider, a.ID)] = label
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"summary": summary, "account_labels": labels})
}
func (s *Server) usagePrices(c *gin.Context) {
	if s.usageDashboard == nil {
		c.JSON(404, gin.H{"error": "usage dashboard disabled"})
		return
	}
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodPut {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
		var rates []usagedashboard.Rate
		if err := c.ShouldBindJSON(&rates); err != nil {
			c.JSON(400, gin.H{"error": "invalid prices"})
			return
		}
		if err := usagedashboard.ValidateRates(rates); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := s.usageDashboard.SaveRates(c.Request.Context(), rates); err != nil {
			log.WithError(err).Warn("usage prices save failed")
			c.JSON(500, gin.H{"error": "prices could not be saved"})
			return
		}
		c.JSON(200, gin.H{"saved": len(rates)})
		return
	}
	rates, err := s.usageDashboard.Rates(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "prices unavailable"})
		return
	}
	c.JSON(200, rates)
}

// CloseUsageDashboard releases the database after the usage dispatcher has drained.
func (s *Server) CloseUsageDashboard() {
	if s.usageDashboard != nil {
		if err := s.usageDashboard.Close(); err != nil {
			log.WithError(err).Warn("usage database close failed")
		}
	}
}

// StartUsageDashboard attaches the persistent sink when the runtime starts.
func (s *Server) StartUsageDashboard() {
	if s.usageDashboard != nil {
		coreusage.RegisterNamedPlugin("usage-dashboard", s.usageDashboard)
	}
}
