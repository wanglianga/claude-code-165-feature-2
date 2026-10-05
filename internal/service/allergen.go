package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"donation-service/internal/domain"
	"donation-service/internal/store"
)

// ---------- 过敏原补录任务 ----------

// createAllergenTaskTx 在事务内生成门店补录任务；同批次已有待处理任务时幂等复用。
func (s *Service) createAllergenTaskTx(ctx context.Context, tx pgx.Tx, batchID, storeID int64,
	reason string, riskGroups []string, createdBy *int64) (*domain.AllergenTask, error) {
	t := &domain.AllergenTask{}
	err := tx.QueryRow(ctx, `
		INSERT INTO allergen_tasks(task_no, batch_id, store_id, reason, risk_groups, created_by)
		VALUES('AT' || to_char(now(),'YYYYMMDD') || '-' || lpad(nextval('allergen_task_no_seq')::text, 5, '0'),
		       $1,$2,$3,$4,$5)
		ON CONFLICT (batch_id) WHERE status IN ('open','review_pending')
		DO UPDATE SET reason=EXCLUDED.reason
		RETURNING id, task_no, batch_id, store_id, reason, risk_groups, status,
		          resolution, review_note, created_by, created_at, resolved_at, resolved_by`,
		batchID, storeID, reason, riskGroups, createdBy).Scan(
		&t.ID, &t.TaskNo, &t.BatchID, &t.StoreID, &t.Reason, &t.RiskGroups, &t.Status,
		&t.Resolution, &t.ReviewNote, &t.CreatedBy, &t.CreatedAt, &t.ResolvedAt, &t.ResolvedBy)
	return t, err
}

func scanAllergenTask(row pgx.Row) (*domain.AllergenTask, error) {
	t := &domain.AllergenTask{}
	err := row.Scan(&t.ID, &t.TaskNo, &t.BatchID, &t.StoreID, &t.Reason, &t.RiskGroups,
		&t.Status, &t.Resolution, &t.ReviewNote, &t.CreatedBy, &t.CreatedAt, &t.ResolvedAt, &t.ResolvedBy)
	return t, err
}

const allergenTaskCols = `id, task_no, batch_id, store_id, reason, risk_groups, status,
	resolution, review_note, created_by, created_at, resolved_at, resolved_by`

// ListAllergenTasks 补录任务列表：门店仅见本店，食安专员/监管/管理员可见全部。
func (s *Service) ListAllergenTasks(ctx context.Context, u *domain.User, storeID *int64, status string) ([]domain.AllergenTask, error) {
	// 权限：门店角色限定本店；机构角色不可见
	switch u.Role {
	case domain.RoleStoreStaff, domain.RoleStoreManager:
		if u.StoreID == nil {
			return nil, domain.ErrForbidden
		}
		sid := *u.StoreID
		storeID = &sid
	case domain.RoleRegulator, domain.RoleAdmin, domain.RoleFoodSafetyOfficer:
	default:
		return nil, domain.ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `
		SELECT `+allergenTaskCols+`
		FROM allergen_tasks
		WHERE ($1::bigint IS NULL OR store_id=$1)
		  AND ($2::text IS NULL OR status=$2)
		ORDER BY id DESC LIMIT 200`, storeID, nilIfEmpty(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AllergenTask
	for rows.Next() {
		var t domain.AllergenTask
		if err := rows.Scan(&t.ID, &t.TaskNo, &t.BatchID, &t.StoreID, &t.Reason, &t.RiskGroups,
			&t.Status, &t.Resolution, &t.ReviewNote, &t.CreatedBy, &t.CreatedAt, &t.ResolvedAt, &t.ResolvedBy); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// markAllergenMissingTx 将批次标记为过敏原标签缺失：阻止发布 + 取消原确认 +
// 幂等生成补录任务（交接前/中由门店或监管触发）。
func (s *Service) markAllergenMissingTx(ctx context.Context, tx pgx.Tx, b *domain.Batch,
	reason string, createdBy *int64) (*domain.AllergenTask, error) {
	riskGroups := domain.HighRiskGroups(b.Allergens)
	if _, groups := domain.SuspectNutsOrDairy(b.Category, b.Description, b.Allergens); len(groups) > 0 {
		riskGroups = groups
	}
	if len(riskGroups) == 0 {
		riskGroups = []string{domain.AllergenGroupNut, domain.AllergenGroupDairy}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE batches SET publish_blocked=TRUE, allergen_confirmed=FALSE,
		    version=version+1, updated_at=now()
		WHERE id=$1`, b.ID); err != nil {
		return nil, err
	}
	return s.createAllergenTaskTx(ctx, tx, b.ID, b.StoreID, reason, riskGroups, createdBy)
}

// ResolveAllergenBlock 门店对过敏原拦截的三选一处置：
//   - supplement：补齐标签（allergens 必填），解除拦截并重算可接收机构
//   - write_off：改为内部报损，批次与生效预约取消
//   - request_review：等待食安专员复核
func (s *Service) ResolveAllergenBlock(ctx context.Context, u *domain.User, batchID int64,
	action string, allergens []string, note string) (*domain.Batch, *domain.AllergenTask, error) {
	switch action {
	case "supplement", "write_off", "request_review":
	default:
		return nil, nil, domain.ErrBadRequest("action 取值: supplement/write_off/request_review")
	}
	if action == "supplement" {
		// 补齐标签：复用补录逻辑（含解除拦截、关闭任务、重算机构）
		if _, _, err := s.ConfirmAllergens(ctx, u, batchID, allergens); err != nil {
			return nil, nil, err
		}
		b, err := getBatch(ctx, s.DB, batchID)
		if err != nil {
			return nil, nil, err
		}
		t, _ := s.latestAllergenTask(ctx, batchID)
		return b, t, nil
	}

	var batch *domain.Batch
	var task *domain.AllergenTask
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		t, err := s.openAllergenTaskTx(ctx, tx, batchID)
		if err != nil {
			return err
		}
		switch action {
		case "write_off":
			if b.Status == domain.BatchHandedOver || b.Status == domain.BatchInDelivery ||
				b.Status == domain.BatchDistributed {
				return domain.ErrConflict("批次已完成交接，不能改为内部报损")
			}
			// 取消生效预约
			var resvID *int64
			var rid int64
			if err := tx.QueryRow(ctx, `
				SELECT id FROM reservations WHERE batch_id=$1 AND status IN ('pending','confirmed')
				ORDER BY id DESC LIMIT 1 FOR UPDATE`, batchID).Scan(&rid); err == nil {
				resvID = &rid
				if _, err := tx.Exec(ctx, `
					UPDATE reservations SET status=$1, cancel_reason='过敏原缺失，门店改为内部报损',
					    version=version+1, updated_at=now()
					WHERE id=$2`, domain.ResvCancelled, rid); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE batches SET status=$1, publish_blocked=FALSE,
				    version=version+1, updated_at=now()
				WHERE id=$2 AND status IN ('reported','reserved','confirmed','handover')`,
				domain.BatchCancelled, batchID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE allergen_tasks SET status=$1, resolution=$2, resolved_at=now(), resolved_by=$3
				WHERE id=$4`, domain.AllergenTaskWriteOff,
				"改为内部报损："+note, u.ID, t.ID); err != nil {
				return err
			}
			detail := map[string]any{
				"batch_no": b.BatchNo, "task_id": t.ID, "note": note,
				"reason": "过敏原标签缺失，门店选择改为内部报损", "handled_by": u.ID,
			}
			exID, err := insertException(ctx, tx, &batchID, resvID, domain.ExInternalWriteOff, detail, &u.ID)
			if err != nil {
				return err
			}
			if err := s.emitExceptionEvent(ctx, tx, exID, &batchID, resvID, domain.ExInternalWriteOff, detail); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "allergen.write_off", map[string]any{
				"batch_id": batchID, "task_id": t.ID, "reservation_id": resvID,
			}); err != nil {
				return err
			}
		case "request_review":
			if t.Status != domain.AllergenTaskOpen {
				return domain.ErrConflict(fmt.Sprintf("任务状态为 %s，不可重复申请复核", t.Status))
			}
			if _, err := tx.Exec(ctx, `
				UPDATE allergen_tasks SET status=$1, review_note=$2
				WHERE id=$3 AND status=$4`,
				domain.AllergenTaskReviewPending, "门店申请食安专员复核："+note, t.ID, domain.AllergenTaskOpen); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "allergen.review_requested", map[string]any{
				"batch_id": batchID, "task_id": t.ID, "requested_by": u.ID,
			}); err != nil {
				return err
			}
		}
		batch, err = getBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		task, err = s.getAllergenTaskTx(ctx, tx, t.ID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, task, nil
}

// ReviewAllergenBlock 食安专员复核过敏原拦截批次：
// approve 解除拦截（视为现场标签合规）；reject 退回门店继续处理。
func (s *Service) ReviewAllergenBlock(ctx context.Context, u *domain.User, batchID int64,
	decision, note string) (*domain.Batch, *domain.AllergenTask, error) {
	if u.Role != domain.RoleFoodSafetyOfficer && u.Role != domain.RoleRegulator && u.Role != domain.RoleAdmin {
		return nil, nil, domain.ErrForbidden
	}
	switch decision {
	case "approve", "reject":
	default:
		return nil, nil, domain.ErrBadRequest("decision 取值: approve/reject")
	}
	var batch *domain.Batch
	var task *domain.AllergenTask
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		t, err := s.openAllergenTaskTx(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if t.Status != domain.AllergenTaskReviewPending {
			return domain.ErrConflict(fmt.Sprintf("任务状态为 %s，不在待复核队列", t.Status))
		}
		if decision == "approve" {
			// 复核通过：视为标签合规，解除拦截；未登记过敏原时按无过敏原处理
			allergens := b.Allergens
			if len(allergens) == 0 {
				allergens = []string{"none"}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE batches SET allergens=$1, allergen_confirmed=TRUE, publish_blocked=FALSE,
				    version=version+1, updated_at=now()
				WHERE id=$2`, allergens, batchID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE allergen_tasks SET status=$1, resolution='食安专员复核通过',
				    review_note=$2, resolved_at=now(), resolved_by=$3
				WHERE id=$4`, domain.AllergenTaskReviewApproved, note, u.ID, t.ID); err != nil {
				return err
			}
		} else {
			// 复核驳回：任务退回门店待处理，批次保持拦截
			if _, err := tx.Exec(ctx, `
				UPDATE allergen_tasks SET status=$1, review_note=$2
				WHERE id=$3`, domain.AllergenTaskOpen, "食安专员复核驳回："+note, t.ID); err != nil {
				return err
			}
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "allergen.reviewed", map[string]any{
			"batch_id": batchID, "task_id": t.ID, "decision": decision, "reviewed_by": u.ID,
		}); err != nil {
			return err
		}
		batch, err = getBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		task, err = s.getAllergenTaskTx(ctx, tx, t.ID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, task, nil
}

// openAllergenTaskTx 返回批次当前待处理（open/review_pending）的补录任务。
func (s *Service) openAllergenTaskTx(ctx context.Context, tx pgx.Tx, batchID int64) (*domain.AllergenTask, error) {
	t, err := scanAllergenTask(tx.QueryRow(ctx, `
		SELECT `+allergenTaskCols+`
		FROM allergen_tasks
		WHERE batch_id=$1 AND status IN ('open','review_pending')
		ORDER BY id DESC LIMIT 1 FOR UPDATE`, batchID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrConflict("该批次没有待处理的过敏原补录任务")
	}
	return t, err
}

func (s *Service) getAllergenTaskTx(ctx context.Context, tx pgx.Tx, id int64) (*domain.AllergenTask, error) {
	return scanAllergenTask(tx.QueryRow(ctx,
		`SELECT `+allergenTaskCols+` FROM allergen_tasks WHERE id=$1`, id))
}

func (s *Service) latestAllergenTask(ctx context.Context, batchID int64) (*domain.AllergenTask, error) {
	t, err := scanAllergenTask(s.DB.QueryRow(ctx, `
		SELECT `+allergenTaskCols+`
		FROM allergen_tasks WHERE batch_id=$1 ORDER BY id DESC LIMIT 1`, batchID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// ---------- 可接收机构重算 ----------

// CharityEligibility 描述某机构对批次的可接收性评估。
type CharityEligibility struct {
	CharityID              int64    `json:"charity_id"`
	CharityName            string   `json:"charity_name"`
	Eligible               bool     `json:"eligible"`
	Reason                 string   `json:"reason,omitempty"`
	AllowedRecipientTypes  []string `json:"allowed_recipient_types"`
	ExcludedRecipientTypes []string `json:"excluded_recipient_types,omitempty"`
}

// EligibleCharities 按批次当前过敏原信息重新计算可接收机构：
// 批次被拦截/未确认时全部不可接收；含高风险过敏原时，儿童、老人、
// 过敏高风险受助点被排除，仅服务敏感人群的机构整体被排除。
func (s *Service) EligibleCharities(ctx context.Context, batchID int64) ([]CharityEligibility, error) {
	b, err := getBatch(ctx, s.DB, batchID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `
		SELECT id, name, recipient_types FROM charities ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CharityEligibility{}
	for rows.Next() {
		var e CharityEligibility
		var types []string
		if err := rows.Scan(&e.CharityID, &e.CharityName, &types); err != nil {
			return nil, err
		}
		allowed, excluded := domain.SplitRecipientTypes(b.Allergens, types)
		e.AllowedRecipientTypes = allowed
		e.ExcludedRecipientTypes = excluded
		switch {
		case b.PublishBlocked || !b.AllergenConfirmed:
			e.Eligible = false
			e.Reason = "批次过敏原信息缺失，已被拦截发布"
		case len(types) > 0 && len(allowed) == 0:
			e.Eligible = false
			e.Reason = fmt.Sprintf("批次含高风险过敏原（%v），机构服务的受助点均为过敏敏感人群",
				domain.HighRiskGroups(b.Allergens))
		default:
			e.Eligible = true
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
