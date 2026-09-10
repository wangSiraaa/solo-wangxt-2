package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type Server struct {
	store *Store
	cache *Cache
	cred  []byte // 本地凭证签名密钥
	now   func() time.Time
}

func NewRouter(s *Server) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	api := r.Group("/api")
	api.GET("/health", s.health)
	api.GET("/overview", s.overview)
	api.POST("/borrows", s.borrow)
	api.POST("/returns", s.returnBorrow)
	api.POST("/quotas", s.adjustQuota)
	api.POST("/admin/reclaim", s.reclaim)
	return r
}

func errJSON(c *gin.Context, code int, errCode, msg string) {
	c.JSON(code, gin.H{"error": errCode, "message": msg})
}

func (s *Server) health(c *gin.Context) {
	if err := s.store.Ping(c.Request.Context()); err != nil {
		errJSON(c, http.StatusServiceUnavailable, "db_down", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "cache": s.cache.Enabled()})
}

// GET /api/overview —— 唯一的缓存读路径,X-Cache 头标识命中情况
func (s *Server) overview(c *gin.Context) {
	ctx := c.Request.Context()
	if ov, hit := s.cache.GetOverview(ctx); hit {
		c.Header("X-Cache", "HIT")
		c.JSON(http.StatusOK, ov)
		return
	}
	ov, err := s.store.Overview(ctx, s.now())
	if err != nil {
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.cache.SetOverview(ctx, ov)
	c.Header("X-Cache", "MISS")
	c.JSON(http.StatusOK, ov)
}

type borrowReq struct {
	PoolID         int64  `json:"pool_id" binding:"required"`
	DepartmentID   int64  `json:"department_id" binding:"required"`
	Employee       string `json:"employee" binding:"required,max=64"`
	Mode           string `json:"mode" binding:"required,oneof=online offline"`
	TTLSeconds     int    `json:"ttl_seconds"`
	IdempotencyKey string `json:"idempotency_key" binding:"required,min=8,max=80"`
}

// POST /api/borrows —— 借用席位。201 新借;200+replay=true 幂等重放;409 额度/席位不足
func (s *Server) borrow(c *gin.Context) {
	var req borrowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	in := BorrowInput{
		PoolID:         req.PoolID,
		DepartmentID:   req.DepartmentID,
		Employee:       req.Employee,
		Mode:           req.Mode,
		IdempotencyKey: req.IdempotencyKey,
	}
	if req.Mode == ModeOffline {
		if req.TTLSeconds < 1 || req.TTLSeconds > 30*24*3600 {
			errJSON(c, http.StatusBadRequest, "bad_request", "offline 模式需要 1s~30d 的 ttl_seconds")
			return
		}
		exp := s.now().Add(time.Duration(req.TTLSeconds) * time.Second)
		nonce, err := newNonce()
		if err != nil {
			errJSON(c, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		in.OfflineExpiresAt = &exp
		in.CredentialNonce = &nonce
	}

	b, replay, err := s.store.Borrow(c.Request.Context(), in)
	switch {
	case errors.Is(err, ErrPoolNotFound):
		errJSON(c, http.StatusNotFound, "pool_not_found", "许可证池不存在")
	case errors.Is(err, ErrNoQuota):
		errJSON(c, http.StatusConflict, "no_quota", "该部门在此许可证池没有额度")
	case errors.Is(err, ErrDeptQuotaExhausted):
		errJSON(c, http.StatusConflict, "dept_quota_exhausted", "部门额度已占满")
	case errors.Is(err, ErrPoolExhausted):
		errJSON(c, http.StatusConflict, "pool_exhausted", "许可证池总席位已占满")
	case err != nil:
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
	}
	if err != nil {
		return
	}

	if !replay {
		s.cache.Invalidate(c.Request.Context())
	}
	body := s.borrowPayload(b, replay)
	if replay {
		c.JSON(http.StatusOK, body) // 网络重试:返回原借用结果
		return
	}
	c.JSON(http.StatusCreated, body)
}

// borrowPayload 离线凭证由 (borrow_id, nonce, exp) 确定性签名,
// 重放时重建出的凭证与首次完全相同。
func (s *Server) borrowPayload(b *Borrow, replay bool) gin.H {
	h := gin.H{"borrow": b, "replay": replay}
	if b.Mode == ModeOffline && b.CredentialNonce != nil && b.OfflineExpiresAt != nil {
		h["credential"] = SignCredential(s.cred, Claims{
			BorrowID:  b.ID,
			PoolID:    b.PoolID,
			Nonce:     *b.CredentialNonce,
			ExpUnixMs: b.OfflineExpiresAt.UnixMilli(),
		})
	}
	return h
}

type returnReq struct {
	BorrowID   int64  `json:"borrow_id"`
	Credential string `json:"credential"`
}

// POST /api/returns —— 归还席位。
// 在线席位:凭 borrow_id 直接归还;离线席位:到期前必须出示有效签名凭证,
// 凭证过期后不再允许主动归还,只能等待过期回收。
// 重复归还(含过期凭证重复归还)返回 200 + result=already_closed,不产生副作用。
func (s *Server) returnBorrow(c *gin.Context) {
	var req returnReq
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx := c.Request.Context()
	now := s.now()

	if req.Credential != "" {
		s.returnOffline(c, req.Credential, now)
		return
	}

	if req.BorrowID == 0 {
		errJSON(c, http.StatusBadRequest, "bad_request", "需要 borrow_id(在线)或 credential(离线)")
		return
	}
	b, err := s.store.borrowByID(ctx, req.BorrowID)
	if errors.Is(err, ErrBorrowNotFound) {
		errJSON(c, http.StatusNotFound, "borrow_not_found", "借用记录不存在")
		return
	}
	if err != nil {
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if b.Status != StatusActive {
		c.JSON(http.StatusOK, gin.H{"result": "already_closed", "borrow": b})
		return
	}
	if b.Mode == ModeOffline {
		errJSON(c, http.StatusForbidden, "credential_required",
			"离线席位在到期前必须凭有效归还凭证释放")
		return
	}
	s.closeAndRespond(c, b.ID, now)
}

func (s *Server) returnOffline(c *gin.Context, token string, now time.Time) {
	ctx := c.Request.Context()
	claims, err := ParseCredential(s.cred, token)
	if err != nil {
		errJSON(c, http.StatusUnauthorized, "invalid_credential", "归还凭证无效或签名不匹配")
		return
	}
	b, err := s.store.borrowByID(ctx, claims.BorrowID)
	if errors.Is(err, ErrBorrowNotFound) {
		errJSON(c, http.StatusNotFound, "borrow_not_found", "借用记录不存在")
		return
	}
	if err != nil {
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if b.Mode != ModeOffline || b.CredentialNonce == nil || *b.CredentialNonce != claims.Nonce {
		errJSON(c, http.StatusUnauthorized, "invalid_credential", "归还凭证与该借用不匹配")
		return
	}
	// 已关闭(归还/回收):幂等返回当前状态,重复归还安全
	if b.Status != StatusActive {
		c.JSON(http.StatusOK, gin.H{"result": "already_closed", "borrow": b})
		return
	}
	// 凭证已过期:不允许再主动归还,等待过期回收
	if b.OfflineExpiresAt != nil && now.After(*b.OfflineExpiresAt) {
		c.JSON(http.StatusGone, gin.H{
			"error":   "credential_expired",
			"message": "归还凭证已过期,席位将由过期回收释放",
			"borrow":  b,
		})
		return
	}
	s.closeAndRespond(c, b.ID, now)
}

func (s *Server) closeAndRespond(c *gin.Context, id int64, now time.Time) {
	b, transitioned, err := s.store.CloseBorrow(c.Request.Context(), id, StatusReturned, now)
	if err != nil {
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !transitioned {
		// 与并发归还/回收竞争失败:返回当前状态,语义幂等
		c.JSON(http.StatusOK, gin.H{"result": "already_closed", "borrow": b})
		return
	}
	s.cache.Invalidate(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"result": "returned", "borrow": b})
}

type quotaReq struct {
	PoolID       int64  `json:"pool_id" binding:"required"`
	DepartmentID int64  `json:"department_id" binding:"required"`
	Quota        int    `json:"quota" binding:"min=0"`
	AdjustedBy   string `json:"adjusted_by" binding:"required,max=64"`
}

// POST /api/quotas —— 部门负责人调整额度。缩减不影响现存借用,只限制新申请。
func (s *Server) adjustQuota(c *gin.Context) {
	var req quotaReq
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	active, err := s.store.AdjustQuota(c.Request.Context(), req.PoolID, req.DepartmentID, req.Quota, req.AdjustedBy)
	if err != nil {
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.cache.Invalidate(c.Request.Context())
	resp := gin.H{
		"pool_id": req.PoolID, "department_id": req.DepartmentID,
		"quota": req.Quota, "active_borrows": active, "adjusted_by": req.AdjustedBy,
	}
	if active > req.Quota {
		resp["note"] = "当前占用已超过新额度:现存借用全部保留,仅限制新的借用申请"
	}
	c.JSON(http.StatusOK, resp)
}

// POST /api/admin/reclaim —— 立即执行一次过期回收(另有后台定时任务)
func (s *Server) reclaim(c *gin.Context) {
	n, err := s.store.ReclaimExpired(c.Request.Context(), s.now())
	if err != nil {
		errJSON(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if n > 0 {
		s.cache.Invalidate(c.Request.Context())
	}
	c.JSON(http.StatusOK, gin.H{"reclaimed": n, "at": s.now().UTC()})
}
