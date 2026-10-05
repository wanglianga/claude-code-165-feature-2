package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"donation-service/internal/domain"
	"donation-service/internal/store"
)

// cancelOpenTasksForBatch 批次终结（报损/售罄/过期）时取消未完成的门店补录任务，
// 并作废等待中的食安复核单。
func cancelOpenTasksForBatch(ctx context.Context, tx txQuerier, batchID int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE store_tasks SET status='cancelled', resolved_at=now()
		WHERE batch_id=$1 AND status='open'`, batchID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE allergen_reviews SET status='rejected',
		    decision_note='批次已终结（取消/过期/报损），复核自动终止', resolved_at=now()
		WHERE batch_id=$1 AND status='pending'`, batchID)
	return err
}

// ---------- 门店任务 ----------

// ListStoreTasks 查询门店补录任务（门店按门店过滤；监管/食安专员/管理员可全量）。
func (s *Service) ListStoreTasks(ctx context.Context, u *domain.User, storeID *int64, status string) ([]domain.StoreTask, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, task_no, batch_id, store_id, type, status, priority, reason,
		       resolution, resolution_by, created_at, resolved_at
		FROM store_tasks
		WHERE ($1::bigint IS NULL OR store_id=$1)
		  AND ($2::text IS NULL OR status=$2)
		ORDER BY (status='open') DESC, id DESC LIMIT 200`, storeID, nilIfEmpty(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.StoreTask{}
	for rows.Next() {
		var t domain.StoreTask
		if err := rows.Scan(&t.ID, &t.TaskNo, &t.BatchID, &t.StoreID, &t.Type, &t.Status,
			&t.Priority, &t.Reason, &t.Resolution, &t.ResolutionBy, &t.CreatedAt, &t.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func getStoreTaskForUpdate(ctx context.Context, tx pgx.Tx, id int64) (*domain.StoreTask, error) {
	t := &domain.StoreTask{}
	err := tx.QueryRow(ctx, `
		SELECT id, task_no, batch_id, store_id, type, status, priority, reason,
		       resolution, resolution_by, created_at, resolved_at
		FROM store_tasks WHERE id=$1 FOR UPDATE`, id).Scan(
		&t.ID, &t.TaskNo, &t.BatchID, &t.StoreID, &t.Type, &t.Status, &t.Priority, &t.Reason,
		&t.Resolution, &t.ResolutionBy, &t.CreatedAt, &t.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ---------- 改为内部报损 ----------

// InternalLoss 门店对过敏原拦截批次选择内部报损：批次取消、不流向公益机构，
// 关闭补录任务并留痕 internal_loss 异常。
func (s *Service) InternalLoss(ctx context.Context, u *domain.User, batchID int64, note string) (*domain.Exception, error) {
	var ex *domain.Exception
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		if !b.AllergenHeld {
			return domain.ErrConflict("仅过敏原缺失被拦截的批次可选择内部报损")
		}
		if b.Status != domain.BatchReported {
			return domain.ErrConflict(fmt.Sprintf("批次当前状态为 %s，不能内部报损", b.Status))
		}
		if err := transitionBatch(ctx, tx, batchID, domain.BatchReported, domain.BatchCancelled); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE store_tasks SET status='done', resolution='internal_loss', resolution_by=$1, resolved_at=now()
			WHERE batch_id=$2 AND status='open'`, u.ID, batchID); err != nil {
			return err
		}
		// 待复核单同时作废
		if _, err := tx.Exec(ctx, `
			UPDATE allergen_reviews SET status='rejected', decision_note=$1, resolved_at=now()
			WHERE batch_id=$2 AND status='pending'`, "门店已改为内部报损，复核终止", batchID); err != nil {
			return err
		}
		detail := map[string]any{
			"reason": "含坚果/乳制品等高风险成分但过敏原无法补录，门店改为内部报损，不进入公益流向",
			"note":   note, "risk_ingredients": b.RiskIngredients, "handled_by": u.ID,
		}
		exID, err := insertException(ctx, tx, &batchID, nil, domain.ExInternalLoss, detail, &u.ID)
		if err != nil {
			return err
		}
		if err := s.emitExceptionEvent(ctx, tx, exID, &batchID, nil, domain.ExInternalLoss, detail); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "batch.internal_loss", map[string]any{
			"batch_id": batchID, "risk_ingredients": b.RiskIngredients,
		}); err != nil {
			return err
		}
		ex, err = s.getException(ctx, tx, exID)
		return err
	})
	return ex, err
}

// ---------- 食安专员复核 ----------

// SubmitAllergenReview 门店无法自行确认过敏原时提交食安复核：任务挂起为“等待复核”。
func (s *Service) SubmitAllergenReview(ctx context.Context, u *domain.User, batchID int64,
	proposedAllergens []string, note string) (*domain.AllergenReview, error) {
	proposedAllergens = domain.NormalizeAllergens(proposedAllergens)
	var review *domain.AllergenReview
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		if !b.AllergenHeld || b.Status != domain.BatchReported {
			return domain.ErrConflict("仅过敏原缺失拦截中的待发布批次可提交食安复核")
		}
		var pending int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM allergen_reviews WHERE batch_id=$1 AND status='pending'`, batchID).
			Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return domain.ErrConflict("该批次已有待处理的食安复核单")
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO allergen_reviews(batch_id, store_id, submitted_by, proposed_allergens, decision_note)
			VALUES($1,$2,$3,$4,$5) RETURNING id`,
			batchID, b.StoreID, u.ID, proposedAllergens, note).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE store_tasks SET status='done', resolution='safety_review', resolution_by=$1, resolved_at=now()
			WHERE batch_id=$2 AND status='open'`, u.ID, batchID); err != nil {
			return err
		}
		if err := insertVoucher(ctx, tx, batchID, nil, domain.VoucherAllergenUpdate, map[string]any{
			"action": "safety_review_submitted", "proposed_allergens": proposedAllergens,
			"note": note, "submitted_by": u.ID,
		}); err != nil {
			return err
		}
		detail := map[string]any{
			"reason": "门店无法确认过敏原，提交食安专员复核", "note": note,
			"proposed_allergens": proposedAllergens, "risk_ingredients": b.RiskIngredients,
			"submitted_by": u.ID,
		}
		exID, err := insertException(ctx, tx, &batchID, nil, domain.ExAllergenReview, detail, &u.ID)
		if err != nil {
			return err
		}
		if err := s.emitExceptionEvent(ctx, tx, exID, &batchID, nil, domain.ExAllergenReview, detail); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "allergen.review_submitted", map[string]any{
			"batch_id": batchID, "review_id": id,
		}); err != nil {
			return err
		}
		review, err = getAllergenReview(ctx, tx, id)
		return err
	})
	return review, err
}

// DecideAllergenReview 食安专员裁决：
// approved  -> 采用复核过敏原，解除拦截并重算可接收机构；
// rejected  -> 维持拦截，重新生成门店补录任务（补齐标签或内部报损）。
func (s *Service) DecideAllergenReview(ctx context.Context, u *domain.User, reviewID int64,
	decision string, allergens []string, note string) (*domain.AllergenReview, *domain.Batch, []int64, error) {
	if decision != domain.ReviewApproved && decision != domain.ReviewRejected {
		return nil, nil, nil, domain.ErrBadRequest("decision 取值: approved/rejected")
	}
	var review *domain.AllergenReview
	var batch *domain.Batch
	var excluded []int64
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		rw, err := lockReview(ctx, tx, reviewID)
		if err != nil {
			return err
		}
		if rw.Status != domain.ReviewPending {
			return domain.ErrConflict(fmt.Sprintf("复核单状态为 %s，已裁决", rw.Status))
		}
		b, err := lockBatch(ctx, tx, rw.BatchID)
		if err != nil {
			return err
		}
		finalAllergens := domain.NormalizeAllergens(allergens)
		if len(finalAllergens) == 0 {
			finalAllergens = rw.ProposedAllergens
		}
		if decision == domain.ReviewApproved {
			if len(finalAllergens) == 0 {
				return domain.ErrBadRequest("核准须给出过敏原列表（无过敏原可填 [\"none\"]）")
			}
			riskGroups := domain.RiskGroupsFromAllergens(finalAllergens)
			if riskGroups == nil {
				riskGroups = []string{}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE allergen_reviews
				SET status='approved', decision_note=$1, reviewed_by=$2, resolved_at=now(),
				    proposed_allergens=$3
				WHERE id=$4`, note, u.ID, finalAllergens, reviewID); err != nil {
				return err
			}
			if b.Status == domain.BatchReported {
				if _, err := tx.Exec(ctx, `
					UPDATE batches
					SET allergens=$1, allergen_confirmed=TRUE, allergen_held=FALSE, hold_reason='',
					    risk_ingredients=$2, version=version+1, updated_at=now()
					WHERE id=$3`, finalAllergens, riskGroups, b.ID); err != nil {
					return err
				}
				eligibleCharities, excludedCharities, rerr := recomputeEligibleCharitiesTx(ctx, tx, b.ID)
				if rerr != nil {
					return rerr
				}
				for _, c := range excludedCharities {
					excluded = append(excluded, c.ID)
				}
				if err := insertVoucher(ctx, tx, b.ID, nil, domain.VoucherAllergenUpdate, map[string]any{
					"action": "review_approved", "allergens": finalAllergens, "risk_groups": riskGroups,
					"eligible_charity_ids": eligibleCharities, "review_id": reviewID,
					"reviewed_by": u.ID, "note": note,
				}); err != nil {
					return err
				}
				if len(excludedCharities) > 0 {
					exclIDs := make([]int64, 0, len(excludedCharities))
					exclNames := make([]string, 0, len(excludedCharities))
					for _, c := range excludedCharities {
						exclIDs = append(exclIDs, c.ID)
						exclNames = append(exclNames, c.Name)
					}
					detail := map[string]any{
						"risk_groups": riskGroups, "review_id": reviewID,
						"reason":                 "食安复核核准后重算可接收机构，高风险受助点机构被排除",
						"excluded_charity_ids":   exclIDs,
						"excluded_charity_names": exclNames,
					}
					exID, ierr := insertException(ctx, tx, &b.ID, nil, domain.ExAllergenExcluded, detail, &u.ID)
					if ierr != nil {
						return ierr
					}
					if ierr := s.emitExceptionEvent(ctx, tx, exID, &b.ID, nil, domain.ExAllergenExcluded, detail); ierr != nil {
						return ierr
					}
				}
			}
			if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", b.ID), "allergen.review_approved", map[string]any{
				"batch_id": b.ID, "review_id": reviewID, "allergens": finalAllergens,
			}); err != nil {
				return err
			}
		} else {
			// 驳回：维持拦截，重新打开门店补录任务
			reason := "食安专员复核驳回"
			if note != "" {
				reason = "食安专员复核驳回：" + note
			}
			if _, err := tx.Exec(ctx, `
				UPDATE allergen_reviews
				SET status='rejected', decision_note=$1, reviewed_by=$2, resolved_at=now()
				WHERE id=$3`, note, u.ID, reviewID); err != nil {
				return err
			}
			if b.Status == domain.BatchReported {
				if err := insertStoreTask(ctx, tx, b.ID, b.StoreID,
					reason+"，请补齐过敏原标签或改为内部报损"); err != nil {
					return err
				}
			}
			if err := insertVoucher(ctx, tx, b.ID, nil, domain.VoucherAllergenUpdate, map[string]any{
				"action": "review_rejected", "review_id": reviewID,
				"reviewed_by": u.ID, "note": note,
			}); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", b.ID), "allergen.review_rejected", map[string]any{
				"batch_id": b.ID, "review_id": reviewID,
			}); err != nil {
				return err
			}
		}
		review, err = getAllergenReview(ctx, tx, reviewID)
		if err != nil {
			return err
		}
		batch, err = getBatch(ctx, tx, b.ID)
		return err
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return review, batch, excluded, nil
}

// ListAllergenReviews 复核单列表（食安专员/监管；门店仅见本店由 service 内过滤交给路由层）。
func (s *Service) ListAllergenReviews(ctx context.Context, u *domain.User, status string) ([]domain.AllergenReview, error) {
	var storeFilter *int64
	if u.Role == domain.RoleStoreStaff || u.Role == domain.RoleStoreManager {
		storeFilter = u.StoreID
	}
	rows, err := s.DB.Query(ctx, `
		SELECT id, batch_id, store_id, status, decision_note, submitted_by, reviewed_by,
		       proposed_allergens, created_at, resolved_at
		FROM allergen_reviews
		WHERE ($1::bigint IS NULL OR store_id=$1)
		  AND ($2::text IS NULL OR status=$2)
		ORDER BY (status='pending') DESC, id DESC LIMIT 200`, storeFilter, nilIfEmpty(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AllergenReview{}
	for rows.Next() {
		var r domain.AllergenReview
		if err := rows.Scan(&r.ID, &r.BatchID, &r.StoreID, &r.Status, &r.DecisionNote,
			&r.SubmittedBy, &r.ReviewedBy, &r.ProposedAllergens, &r.CreatedAt, &r.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func getAllergenReview(ctx context.Context, q txQuerier, id int64) (*domain.AllergenReview, error) {
	r := &domain.AllergenReview{}
	err := q.QueryRow(ctx, `
		SELECT id, batch_id, store_id, status, decision_note, submitted_by, reviewed_by,
		       proposed_allergens, created_at, resolved_at
		FROM allergen_reviews WHERE id=$1`, id).Scan(
		&r.ID, &r.BatchID, &r.StoreID, &r.Status, &r.DecisionNote, &r.SubmittedBy, &r.ReviewedBy,
		&r.ProposedAllergens, &r.CreatedAt, &r.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func lockReview(ctx context.Context, tx pgx.Tx, id int64) (*domain.AllergenReview, error) {
	r := &domain.AllergenReview{}
	err := tx.QueryRow(ctx, `
		SELECT id, batch_id, store_id, status, decision_note, submitted_by, reviewed_by,
		       proposed_allergens, created_at, resolved_at
		FROM allergen_reviews WHERE id=$1 FOR UPDATE`, id).Scan(
		&r.ID, &r.BatchID, &r.StoreID, &r.Status, &r.DecisionNote, &r.SubmittedBy, &r.ReviewedBy,
		&r.ProposedAllergens, &r.CreatedAt, &r.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ---------- 可接收机构查询 ----------

type CharityEligibility struct {
	Charity  domain.Charity `json:"charity"`
	Eligible bool           `json:"eligible"`
	Reason   string         `json:"reason,omitempty"`
}

// ListEligibleCharities 返回批次过敏原重算后的可接收机构及排除原因（门店/机构/监管可见）。
func (s *Service) ListEligibleCharities(ctx context.Context, batchID int64) (*domain.Batch, []CharityEligibility, error) {
	b, err := getBatch(ctx, s.DB, batchID)
	if err != nil {
		return nil, nil, err
	}
	charities, err := listCharities(ctx, s.DB)
	if err != nil {
		return nil, nil, err
	}
	groups := domain.RiskGroupsFromAllergens(b.Allergens)
	out := make([]CharityEligibility, 0, len(charities))
	for _, c := range charities {
		item := CharityEligibility{Charity: c, Eligible: true}
		if len(groups) > 0 && c.HighRiskRecipients && !c.AllergenManaged {
			item.Eligible = false
			item.Reason = fmt.Sprintf("该机构服务儿童、老人或过敏高风险受助点，且无过敏原分区储运备案，不可接收含%s批次",
				strings.Join(domain.RiskGroupLabels(groups), "/"))
		}
		out = append(out, item)
	}
	return b, out, nil
}
