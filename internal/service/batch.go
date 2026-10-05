package service

import (
	"context"
	"fmt"
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
// 过敏原缺失拦截：食品疑似含坚果/乳制品但未填写过敏原时，批次被阻止发布给
// 公益机构（publish_blocked=true，机构不可见不可约），并生成门店补录任务。
func (s *Service) ReportBatch(ctx context.Context, u *domain.User, storeID int64, in ReportBatchInput) (*domain.Batch, []domain.Box, *domain.AllergenTask, error) {
	if err := RequireStore(u, storeID); err != nil {
		return nil, nil, nil, err
	}
	if in.Category == "" || in.TotalBoxes <= 0 || in.ShelfLifeHours <= 0 {
		return nil, nil, nil, domain.ErrBadRequest("品类、箱数、保质时长为必填且必须为正数")
	}
	switch in.Packaging {
	case "", "intact", "minor_damage", "damaged":
	default:
		return nil, nil, nil, domain.ErrBadRequest("包装完整度取值: intact/minor_damage/damaged")
	}
	if in.Packaging == "" {
		in.Packaging = "intact"
	}
	if in.Packaging == "damaged" {
		return nil, nil, nil, domain.ErrUnprocessable("包装破损的食品不允许进入捐赠流程")
	}
	now := time.Now()
	if in.ProducedAt.After(now) {
		return nil, nil, nil, domain.ErrBadRequest("生产时间不能晚于当前时间")
	}
	expiresAt := in.ProducedAt.Add(time.Duration(in.ShelfLifeHours) * time.Hour)
	if !in.LatestPickupAt.After(now) {
		return nil, nil, nil, domain.ErrUnprocessable("最晚取走时间必须晚于当前时间")
	}
	if in.LatestPickupAt.After(expiresAt) {
		return nil, nil, nil, domain.ErrUnprocessable("最晚取走时间不能超过保质期末（生产时间+保质时长）")
	}
	if in.RequiresColdChain && in.StorageTempMaxC == nil {
		return nil, nil, nil, domain.ErrBadRequest("需冷链批次必须给出存储温度上限 storage_temp_max_c")
	}
	if in.AllergenConfirmed && len(in.Allergens) == 0 {
		return nil, nil, nil, domain.ErrBadRequest("确认过敏原信息时须给出过敏原列表（无过敏原可填 [\"none\"]）")
	}
	// 疑似含坚果/乳制品但未填写过敏原 -> 拦截发布 + 生成补录任务
	suspect, riskGroups := domain.SuspectNutsOrDairy(in.Category, in.Description, in.Allergens)
	blocked := suspect && !in.AllergenConfirmed

	var batch *domain.Batch
	var boxes []domain.Box
	var task *domain.AllergenTask
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		var id int64
		var batchNo string
		err := tx.QueryRow(ctx, `
			INSERT INTO batches(batch_no, store_id, category, description, total_boxes, quantity_desc,
			    produced_at, shelf_life_hours, expires_at, allergens, allergen_confirmed, publish_blocked,
			    requires_cold_chain, storage_temp_max_c, packaging, address, lat, lng,
			    latest_pickup_at, notes)
			VALUES('B' || to_char(now(),'YYYYMMDD') || '-' || lpad(nextval('batch_no_seq')::text, 5, '0'),
			    $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			RETURNING id, batch_no`,
			storeID, in.Category, in.Description, in.TotalBoxes, in.QuantityDesc,
			in.ProducedAt, in.ShelfLifeHours, expiresAt, in.Allergens, in.AllergenConfirmed, blocked,
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
			"allergen_confirmed": in.AllergenConfirmed, "publish_blocked": blocked,
			"requires_cold_chain": in.RequiresColdChain,
			"packaging":           in.Packaging, "latest_pickup_at": in.LatestPickupAt,
			"reported_by": u.ID,
		}); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", id), "batch.reported", map[string]any{
			"batch_id": id, "batch_no": batchNo, "store_id": storeID, "total_boxes": in.TotalBoxes,
			"publish_blocked": blocked,
		}); err != nil {
			return err
		}
		if blocked {
			// 生成门店补录任务 + allergen_missing 异常（同一事务，保证可审计）
			reason := fmt.Sprintf("食品疑似含高风险过敏原（%v）但未填写过敏原信息，已阻止发布给公益机构", riskGroups)
			t, err := s.createAllergenTaskTx(ctx, tx, id, storeID, reason, riskGroups, &u.ID)
			if err != nil {
				return err
			}
			task = t
			detail := map[string]any{
				"batch_no": batchNo, "risk_groups": riskGroups, "stage": "report",
				"task_id": t.ID, "reason": reason, "reported_by": u.ID,
			}
			exID, err := insertException(ctx, tx, &id, nil, domain.ExAllergenMissing, detail, &u.ID)
			if err != nil {
				return err
			}
			if err := s.emitExceptionEvent(ctx, tx, exID, &id, nil, domain.ExAllergenMissing, detail); err != nil {
				return err
			}
			if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", id), "allergen.task_created", map[string]any{
				"batch_id": id, "task_id": t.ID, "task_no": t.TaskNo, "risk_groups": riskGroups,
			}); err != nil {
				return err
			}
		}
		batch, err = getBatch(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return batch, boxes, task, nil
}

// ConfirmAllergens 门店补充确认过敏原信息（补齐标签）：解除发布拦截、
// 关闭补录任务，并重新计算该批次可接收机构（儿童/老人/过敏高风险受助点可能被排除）。
func (s *Service) ConfirmAllergens(ctx context.Context, u *domain.User, batchID int64, allergens []string) (*domain.Batch, []CharityEligibility, error) {
	if len(allergens) == 0 {
		return nil, nil, domain.ErrBadRequest("过敏原列表不能为空（无过敏原可填 [\"none\"]）")
	}
	var batch *domain.Batch
	err := store.WithTx(ctx, s.DB, func(tx pgx.Tx) error {
		b, err := lockBatch(ctx, tx, batchID)
		if err != nil {
			return err
		}
		if err := RequireStore(u, b.StoreID); err != nil {
			return err
		}
		switch b.Status {
		case domain.BatchReported, domain.BatchReserved, domain.BatchConfirmed, domain.BatchHandover:
			// 上报后至交接完成前均可补录（交接中补齐标签场景）
		default:
			return domain.ErrConflict(fmt.Sprintf("批次当前状态为 %s，不可补录过敏原", b.Status))
		}
		if _, err := tx.Exec(ctx, `
			UPDATE batches SET allergens=$1, allergen_confirmed=TRUE, publish_blocked=FALSE,
			    version=version+1, updated_at=now()
			WHERE id=$2`, allergens, batchID); err != nil {
			return err
		}
		// 关闭待处理的补录任务
		if _, err := tx.Exec(ctx, `
			UPDATE allergen_tasks SET status=$1, resolution='门店已补齐过敏原标签',
			    resolved_at=now(), resolved_by=$2
			WHERE batch_id=$3 AND status IN ('open','review_pending')`,
			domain.AllergenTaskSupplemented, u.ID, batchID); err != nil {
			return err
		}
		if err := insertOutbox(ctx, tx, s.Cfg.KafkaTopic, fmt.Sprintf("batch:%d", batchID), "allergen.supplemented", map[string]any{
			"batch_id": batchID, "allergens": allergens, "confirmed_by": u.ID,
		}); err != nil {
			return err
		}
		batch, err = getBatch(ctx, tx, batchID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	// 补录后重算可接收机构
	eligible, err := s.EligibleCharities(ctx, batchID)
	if err != nil {
		return nil, nil, err
	}
	return batch, eligible, nil
}

// ListBatches 按角色过滤批次列表。被过敏原拦截（publish_blocked）的批次
// 对公益机构不可见——即“阻止发布给公益机构”。
func (s *Service) ListBatches(ctx context.Context, u *domain.User, status string, storeID *int64) ([]domain.Batch, error) {
	hideBlocked := u.Role == domain.RoleCharityStaff
	rows, err := s.DB.Query(ctx, `
		SELECT id, batch_no, store_id, status, category, description, total_boxes, quantity_desc,
		       produced_at, shelf_life_hours, expires_at, allergens, allergen_confirmed, publish_blocked,
		       requires_cold_chain, storage_temp_max_c, packaging, address, lat, lng,
		       latest_pickup_at, notes, version, created_at, updated_at
		FROM batches
		WHERE ($1::text IS NULL OR status=$1)
		  AND ($2::bigint IS NULL OR store_id=$2)
		  AND (NOT $3::boolean OR publish_blocked = FALSE)
		ORDER BY id DESC LIMIT 200`, nilIfEmpty(status), storeID, hideBlocked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Batch
	for rows.Next() {
		var b domain.Batch
		if err := rows.Scan(&b.ID, &b.BatchNo, &b.StoreID, &b.Status, &b.Category, &b.Description,
			&b.TotalBoxes, &b.QuantityDesc, &b.ProducedAt, &b.ShelfLifeHours, &b.ExpiresAt,
			&b.Allergens, &b.AllergenConfirmed, &b.PublishBlocked, &b.RequiresColdChain, &b.StorageTempMaxC,
			&b.Packaging, &b.Address, &b.Lat, &b.Lng, &b.LatestPickupAt, &b.Notes,
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
