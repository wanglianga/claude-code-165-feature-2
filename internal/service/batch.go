package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"donation-service/internal/domain"
	"donation-service/internal/store"
)

// ReportBatchInput 门店上报临期食品批次的输入。
type ReportBatchInput struct {
	Category          string
	Description       string
	TotalBoxes        int
	QuantityDesc      string
	ProducedAt        time.Time
	ShelfLifeHours    int
	Allergens         []string
	AllergenConfirmed bool
	RequiresColdChain bool
	StorageTempMaxC   *float64
	Packaging         string
	Address           string
	Lat               float64
	Lng               float64
	LatestPickupAt    time.Time
	Notes             string
}

// ReportBatch 门店闭店前上报可捐赠批次：落库 + 生成箱号 + 来源凭证 + 事件。
func (s *Service) ReportBatch(ctx context.Context, u *domain.User, storeID int64, in ReportBatchInput) (*domain.Batch, []domain.Box, error) {
	if err := RequireStore(u, storeID); err != nil {
		return nil, nil, err
	}
	if in.Category == "" || in.TotalBoxes <= 0 || in.ShelfLifeHours <= 0 {
		return nil, nil, domain.ErrBadRequest("品类、箱数、保质时长为必填且必须为正数")
	}
	switch in.Packaging {
	case "", "intact", "minor_damage", "damaged":
	default:
		return nil, nil, domain.ErrBadRequest("包装完整度取值: intact/minor_damage/damaged")
	}
	if in.Packaging == "" {
		in.Packaging = "intact"
	}
	if in.Packaging == "damaged" {
		return nil, nil, domain.ErrUnprocessable("包装破损的食品不允许进入捐赠流程")
	}
	now := time.Now()
	if in.ProducedAt.After(now) {
		return nil, nil, domain.ErrBadRequest("生产时间不能晚于当前时间")
	}
	expiresAt := in.ProducedAt.Add(time.Duration(in.ShelfLifeHours) * time.Hour)
	if !in.LatestPickupAt.After(now) {
		return nil, nil, domain.ErrUnprocessable("最晚取走时间必须晚于当前时间")
	}
	if in.LatestPickupAt.After(expiresAt) {
		return nil, nil, domain.ErrUnprocessable("最晚取走时间不能超过保质期末（生产时间+保质时长）")
	}
	if in.RequiresColdChain && in.StorageTempMaxC == nil {
		return nil, nil, domain.ErrBadRequest("需冷链批次必须给出存储温度上限 storage_temp_max_c")
	}
	in.Allergens = domain.NormalizeAllergens(in.Allergens)
	if in.Allergens == nil {
		in.Allergens = []string{} // pgx 对 nil 切片写 NULL，而 allergens 列为 NOT NULL
	}
	if in.AllergenConfirmed && len(in.Allergens) == 0 {
		return nil, nil, domain.ErrBadRequest("确认过敏原信息时须给出过敏原列表（无过敏原可填 [\"none\"]）")
	}
	// 过敏原缺失拦截：未填写过敏原时，根据品类/描述推断是否含坚果、乳制品等高风险成分。
	riskGroups := []string{}
	if in.AllergenConfirmed {
		riskGroups = domain.RiskGroupsFromAllergens(in.Allergens)
	} else {
		riskGroups = domain.DetectRiskIngredients(in.Category, in.Description, nil, false)
	}
	held := !in.AllergenConfirmed && len(riskGroups) > 0
	if riskGroups == nil {
		riskGroups = []string{} // NOT NULL 列禁止 nil 切片
	}
	holdReason := ""
	if held {
		holdReason = fmt.Sprintf("食品含%s等高风险成分但未填写过敏原信息，批次已拦截，须补录过敏原标签或选择内部报损/食安复核",
			strings.Join(domain.RiskGroupLabels(riskGroups), "、"))
	}

	var batch *domain.Batch
	var boxes []domain.Box
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		var id int64
		var batchNo string
		err := tx.QueryRow(ctx, `
			INSERT INTO batches(batch_no, store_id, category, description, total_boxes, quantity_desc,
			    produced_at, shelf_life_hours, expires_at, allergens, allergen_confirmed,
			    allergen_held, hold_reason, risk_ingredients,
			    requires_cold_chain, storage_temp_max_c, packaging, address, lat, lng,
			    latest_pickup_at, notes)
			VALUES('B' || to_char(now(),'YYYYMMDD') || '-' || lpad(nextval('batch_no_seq')::text, 5, '0'),
			    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
			RETURNING id, batch_no`,
			storeID, in.Category, in.Description, in.TotalBoxes, in.QuantityDesc,
			in.ProducedAt, in.ShelfLifeHours, expiresAt, in.Allergens, in.AllergenConfirmed,
			held, holdReason, riskGroups,
			in.RequiresColdChain, in.StorageTempMaxC, in.Packaging, in.Address, in.Lat, in.Lng,
			in.LatestPickupAt, in.Notes).Scan(&id, &batchNo)
		if err != nil {
			return err
		}
		for i := 1; i <= in.TotalBoxes; i++ {
			boxNo := fmt.Sprintf("%s-BOX-%03d", batchNo, i)
			var boxID int64
			var createdAt time.Time
			if err := tx.QueryRow(ctx, `
				INSERT INTO boxes(batch_id, box_no) VALUES($1,$2) RETURNING id, created_at`,
				id, boxNo).Scan(&boxID, &createdAt); err != nil {
				return err
			}
			boxes = append(boxes, domain.Box{ID: boxID, BatchID: id, BoxNo: boxNo, Status: domain.BoxPending, CreatedAt: createdAt})
		}
		if err := insertVoucher(ctx, tx, id, nil, domain.VoucherSource, map[string]any{
			"batch_no": batchNo, "store_id": storeID, "category": in.Category,
			"total_boxes": in.TotalBoxes, "produced_at": in.ProducedAt,
			"expires_at": expiresAt, "allergens": in.Allergens,
			"allergen_confirmed": in.AllergenConfirmed, "allergen_held": held,
			"risk_ingredients": riskGroups, "requires_cold_chain": in.RequiresColdChain,
			"packaging": in.Packaging, "latest_pickup_at": in.LatestPickupAt,
			"reported_by": u.ID,
		}); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", id), "batch.reported", map[string]any{
			"batch_id": id, "batch_no": batchNo, "store_id": storeID, "total_boxes": in.TotalBoxes,
			"allergen_held": held, "risk_ingredients": riskGroups,
		}); err != nil {
			return err
		}
		// 已确认且含坚果/乳制品：上报即重算可接收机构（含高风险受助点排除）。
		if in.AllergenConfirmed && len(riskGroups) > 0 {
			if _, _, err := recomputeEligibleCharitiesTx(ctx, tx, id); err != nil {
				return err
			}
		}
		if held {
			// 拦截批次：生成门店高优待办 + allergen_missing 异常凭证，批次不发布给公益机构。
			if err := insertStoreTask(ctx, tx, id, storeID, holdReason); err != nil {
				return err
			}
			detail := map[string]any{
				"reason": holdReason, "risk_ingredients": riskGroups,
				"category": in.Category, "description": in.Description, "reported_by": u.ID,
			}
			exID, err := insertException(ctx, tx, &id, nil, domain.ExAllergenMissing, detail, &u.ID)
			if err != nil {
				return err
			}
			if err := s.emitExceptionEvent(ctx, tx, exID, &id, nil, domain.ExAllergenMissing, detail); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", id), "allergen.held", map[string]any{
				"batch_id": id, "risk_ingredients": riskGroups,
			}); err != nil {
				return err
			}
		}
		batch, err = getBatch(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, boxes, nil
}

// ConfirmAllergens 门店补录过敏原标签：解除高风险成分拦截、关闭补录任务，
// 并重新计算可接收机构（儿童/老人/过敏高风险受助点机构可能被排除）。
func (s *Service) ConfirmAllergens(ctx context.Context, u *domain.User, batchID int64, allergens []string) (*domain.Batch, []int64, error) {
	allergens = domain.NormalizeAllergens(allergens)
	if len(allergens) == 0 {
		return nil, nil, domain.ErrBadRequest("过敏原列表不能为空（无过敏原可填 [\"none\"]）")
	}
	var batch *domain.Batch
	var excluded []int64
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		switch b.Status {
		case domain.BatchCancelled, domain.BatchExpired:
			return domain.ErrConflict(fmt.Sprintf("批次已 %s，不能补录过敏原", b.Status))
		case domain.BatchReported:
			// 正常补录解除拦截
		default:
			// 非待预约状态仅更新信息，不改变拦截/资格（兼容旧有行为）
			if _, err := tx.Exec(ctx, `
				UPDATE batches SET allergens=$1, version=version+1, updated_at=now() WHERE id=$2`,
				allergens, batchID); err != nil {
				return err
			}
			batch, err = getBatch(ctx, tx, batchID)
			return err
		}
		riskGroups := domain.RiskGroupsFromAllergens(allergens)
		if riskGroups == nil {
			riskGroups = []string{}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE batches
			SET allergens=$1, allergen_confirmed=TRUE, allergen_held=FALSE, hold_reason='',
			    risk_ingredients=$2, version=version+1, updated_at=now()
			WHERE id=$3`, allergens, riskGroups, batchID); err != nil {
			return err
		}
		eligibleCharities, excludedCharities, err := recomputeEligibleCharitiesTx(ctx, tx, batchID)
		if err != nil {
			return err
		}
		// 关闭补录任务（门店选择：补齐标签）
		if _, err := tx.Exec(ctx, `
			UPDATE store_tasks SET status='done', resolution='supplement', resolution_by=$1, resolved_at=now()
			WHERE batch_id=$2 AND status='open'`, u.ID, batchID); err != nil {
			return err
		}
		if err := insertVoucher(ctx, tx, batchID, nil, domain.VoucherAllergenUpdate, map[string]any{
			"action": "supplement", "allergens": allergens, "risk_groups": riskGroups,
			"eligible_charity_ids": eligibleCharities,
			"excluded_count":       len(excludedCharities), "updated_by": u.ID,
		}); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "allergen.supplemented", map[string]any{
			"batch_id": batchID, "allergens": allergens, "risk_groups": riskGroups,
			"eligible_charity_ids": eligibleCharities,
		}); err != nil {
			return err
		}
		if len(excludedCharities) > 0 {
			exclIDs := make([]int64, 0, len(excludedCharities))
			exclNames := make([]string, 0, len(excludedCharities))
			for _, c := range excludedCharities {
				excluded = append(excluded, c.ID)
				exclIDs = append(exclIDs, c.ID)
				exclNames = append(exclNames, c.Name)
			}
			detail := map[string]any{
				"risk_groups":            riskGroups,
				"reason":                 "补录过敏原后重算可接收机构：服务儿童、老人或过敏高风险受助点且无过敏原管理备案的机构被排除",
				"excluded_charity_ids":   exclIDs,
				"excluded_charity_names": exclNames,
				"resolved_by":            u.ID,
			}
			exID, err := insertException(ctx, tx, &batchID, nil, domain.ExAllergenExcluded, detail, &u.ID)
			if err != nil {
				return err
			}
			if err := s.emitExceptionEvent(ctx, tx, exID, &batchID, nil, domain.ExAllergenExcluded, detail); err != nil {
				return err
			}
		}
		batch, err = getBatch(ctx, tx, batchID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, excluded, nil
}

// recomputeEligibleCharitiesTx 依据批次过敏原与机构风险标志重算可接收机构快照。
// 不含坚果/乳制品的批次不做限制（eligible_charity_ids 置 NULL）。
func recomputeEligibleCharitiesTx(ctx context.Context, tx pgx.Tx, batchID int64) ([]int64, []domain.Charity, error) {
	b, err := getBatch(ctx, tx, batchID)
	if err != nil {
		return nil, nil, err
	}
	charities, err := listCharities(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	groups := domain.RiskGroupsFromAllergens(b.Allergens)
	if len(groups) == 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE batches SET eligible_charity_ids=NULL, version=version+1, updated_at=now() WHERE id=$1`,
			batchID); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	eligible := make([]int64, 0, len(charities))
	var excluded []domain.Charity
	for _, c := range charities {
		if domain.CharityAllergenEligible(*b, c) {
			eligible = append(eligible, c.ID)
		} else {
			excluded = append(excluded, c)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE batches SET eligible_charity_ids=$1, version=version+1, updated_at=now() WHERE id=$2`,
		eligible, batchID); err != nil {
		return nil, nil, err
	}
	return eligible, excluded, nil
}

// insertStoreTask 生成过敏原补录待办（同批次仅一条 open，防重）。
func insertStoreTask(ctx context.Context, tx pgx.Tx, batchID, storeID int64, reason string) error {
	var exists int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM store_tasks WHERE batch_id=$1 AND status='open'`, batchID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO store_tasks(task_no, batch_id, store_id, type, status, priority, reason)
		VALUES('T' || to_char(now(),'YYYYMMDD') || '-' || lpad(nextval('task_no_seq')::text, 6, '0'),
		       $1, $2, 'allergen_supplement', 'open', 'high', $3)`,
		batchID, storeID, reason)
	return err
}

// ListBatches 按角色过滤批次列表。
func (s *Service) ListBatches(ctx context.Context, u *domain.User, status string, storeID *int64) ([]domain.Batch, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, batch_no, store_id, status, category, description, total_boxes, quantity_desc,
		       produced_at, shelf_life_hours, expires_at, allergens, allergen_confirmed,
		       allergen_held, hold_reason, risk_ingredients, eligible_charity_ids,
		       requires_cold_chain, storage_temp_max_c, packaging, address, lat, lng,
		       latest_pickup_at, notes, version, created_at, updated_at
		FROM batches
		WHERE ($1::text IS NULL OR status=$1)
		  AND ($2::bigint IS NULL OR store_id=$2)
		ORDER BY id DESC LIMIT 200`, nilIfEmpty(status), storeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Batch
	for rows.Next() {
		var b domain.Batch
		if err := rows.Scan(&b.ID, &b.BatchNo, &b.StoreID, &b.Status, &b.Category, &b.Description,
			&b.TotalBoxes, &b.QuantityDesc, &b.ProducedAt, &b.ShelfLifeHours, &b.ExpiresAt,
			&b.Allergens, &b.AllergenConfirmed, &b.AllergenHeld, &b.HoldReason, &b.RiskIngredients,
			&b.EligibleCharityIDs,
			&b.RequiresColdChain, &b.StorageTempMaxC, &b.Packaging,
			&b.Address, &b.Lat, &b.Lng, &b.LatestPickupAt, &b.Notes,
			&b.Version, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetBatchDetail 批次详情：批次 + 箱 + 当前预约。
func (s *Service) GetBatchDetail(ctx context.Context, batchID int64) (*domain.Batch, []domain.Box, *domain.Reservation, error) {
	b, err := getBatch(ctx, s.DB, batchID)
	if err != nil {
		return nil, nil, nil, err
	}
	boxes, err := s.listBoxes(ctx, batchID)
	if err != nil {
		return nil, nil, nil, err
	}
	var resv *domain.Reservation
	rows, err := s.DB.Query(ctx, `
		SELECT id FROM reservations WHERE batch_id=$1 ORDER BY id DESC LIMIT 1`, batchID)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	if rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, nil, err
		}
		resv, err = getReservation(ctx, s.DB, id)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return b, boxes, resv, nil
}

func (s *Service) listBoxes(ctx context.Context, batchID int64) ([]domain.Box, error) {
	rows, err := s.DB.Query(ctx,
		`SELECT id, batch_id, box_no, status, created_at FROM boxes WHERE batch_id=$1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Box
	for rows.Next() {
		var b domain.Box
		if err := rows.Scan(&b.ID, &b.BatchID, &b.BoxNo, &b.Status, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
