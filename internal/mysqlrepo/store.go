package mysqlrepo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"seckill_lite/internal/model"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrSoldOut        = errors.New("sold out")
	ErrLimitExceeded  = errors.New("limit exceeded")
	ErrPayAfterCancel = errors.New("pay after cancel")
	ErrInconsistent   = errors.New("inconsistent durable state")
	ErrItemNotFound   = errors.New("item not found")
)

type Store struct {
	db          *sql.DB
	expireAfter time.Duration
}

// ReservationInput 对应一次 Redis 实际预扣；消息重投沿用预约 ID，另一次预扣使用新 ID。
type ReservationInput struct {
	ReservationID  string
	UserID         int64
	IdempotencyKey string
	ItemID         int64
	Quantity       int
	PayloadHash    string
}

// ReservationOutcome 告诉 Worker 下一步是回填订单、补偿预约还是直接结束；
// 只有事务提交返回成功后才交给调用方，Redis 操作与 Stream ACK 不在本地事务内。
type ReservationOutcome struct {
	Kind             string
	Order            *model.Order
	Reason           string
	Compensate       bool
	CompensationDone bool
}

func Open(dsn string, expireAfter time.Duration) (*Store, error) {
	db, err := sql.Open("mysql", withUTCTimezone(dsn))
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(16)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if expireAfter <= 0 {
		expireAfter = 15 * time.Minute
	}
	return &Store{db: db, expireAfter: expireAfter}, nil
}

// withUTCTimezone 默认统一数据库会话时区，使数据库时间与代码中的 UTC 时间一致。
func withUTCTimezone(dsn string) string {
	if strings.Contains(dsn, "time_zone=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "time_zone=%27%2B00%3A00%27"
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// withTx 统一事务边界：业务函数报错则尝试回滚，成功后才提交；本层不自动重试。
// Commit 返回错误不能证明事务未提交，调用方不能据此直接回补 Redis 库存。
// 隔离级别沿用数据库默认设置；在 REPEATABLE READ 下需区分快照读与当前读。
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// CreateActivity 将活动及全部商品库存作为一个事务创建，任一商品写入失败则回滚。
// 提交成功只代表 MySQL 初始化完成；Redis 库存由 service 层随后用 SET NX 初始化，
// 不存在跨 MySQL/Redis 的原子提交，也不能用本方法覆盖已有活动的在途预扣。
func (s *Store) CreateActivity(ctx context.Context, in model.ActivityInput) (*model.Activity, []model.Item, error) {
	var activity *model.Activity
	items := make([]model.Item, 0, len(in.Items))
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO activities (name, start_at, end_at, status)
			VALUES (?, ?, ?, 'ACTIVE')`, in.Name, in.StartAt.UTC(), in.EndAt.UTC())
		if err != nil {
			return fmt.Errorf("insert activity: %w", err)
		}
		activityID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("activity id: %w", err)
		}
		for _, item := range in.Items {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO seckill_items
					(activity_id, sku_code, name, price_cents, total_stock, available_stock, per_user_limit)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				activityID, item.SKUCode, item.Name, item.PriceCents,
				item.TotalStock, item.TotalStock, item.PerUserLimit)
			if err != nil {
				return fmt.Errorf("insert item: %w", err)
			}
			itemID, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("item id: %w", err)
			}
			items = append(items, model.Item{
				ID:             itemID,
				ActivityID:     activityID,
				SKUCode:        item.SKUCode,
				Name:           item.Name,
				PriceCents:     item.PriceCents,
				TotalStock:     item.TotalStock,
				AvailableStock: item.TotalStock,
				PerUserLimit:   item.PerUserLimit,
				ActivityStart:  in.StartAt.UTC(),
				ActivityEnd:    in.EndAt.UTC(),
				ActivityStatus: "ACTIVE",
			})
		}
		activity = &model.Activity{
			ID:        activityID,
			Name:      in.Name,
			StartAt:   in.StartAt.UTC(),
			EndAt:     in.EndAt.UTC(),
			Status:    "ACTIVE",
			CreatedAt: time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return activity, items, nil
}

func (s *Store) GetItem(ctx context.Context, itemID int64) (*model.Item, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT i.id, i.activity_id, i.sku_code, i.name, i.price_cents, i.total_stock,
		       i.available_stock, i.per_user_limit, i.version,
		       a.start_at, a.end_at, a.status
		  FROM seckill_items i
		  JOIN activities a ON a.id = i.activity_id
		 WHERE i.id = ?`, itemID)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrItemNotFound
	}
	if err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Store) GetActivityStock(ctx context.Context, activityID int64) (*model.Activity, []model.Item, error) {
	var activity model.Activity
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, start_at, end_at, status, created_at
		  FROM activities WHERE id = ?`, activityID).
		Scan(&activity.ID, &activity.Name, &activity.StartAt, &activity.EndAt, &activity.Status, &activity.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get activity: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.activity_id, i.sku_code, i.name, i.price_cents, i.total_stock,
		       i.available_stock, i.per_user_limit, i.version,
		       a.start_at, a.end_at, a.status
		  FROM seckill_items i
		  JOIN activities a ON a.id = i.activity_id
		 WHERE i.activity_id = ?
		 ORDER BY i.id`, activityID)
	if err != nil {
		return nil, nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()
	items := make([]model.Item, 0)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate items: %w", err)
	}
	return &activity, items, nil
}

// LookupRequest 为幂等回放和轮询读取持久结果；订单状态优先，拒绝原因来自请求终态。
// 两条 SELECT 当前不在同一事务快照中：若 Worker 恰在中间提交，可能读到
// “无订单 + SUCCEEDED”，上层会暂时报 RESULT_UNKNOWN；此处尚未重读消除该窗口。
func (s *Store) LookupRequest(ctx context.Context, userID int64, key string) (*model.Order, *model.RequestResult, error) {
	order, err := s.getOrderByUserKey(ctx, userID, key)
	if errors.Is(err, sql.ErrNoRows) {
		order = nil
	} else if err != nil {
		return nil, nil, err
	}
	req, err := s.getRequestResult(ctx, userID, key)
	if err != nil {
		return nil, nil, err
	}
	return order, req, nil
}

func (s *Store) GetOrderByNo(ctx context.Context, orderNo string, userID int64) (*model.Order, error) {
	query := `SELECT id, order_no, user_id, activity_id, item_id, quantity, amount_cents,
	                 idempotency_key, payload_hash, reservation_id, status, expire_at,
	                 paid_at, cancelled_at, created_at
	            FROM orders WHERE order_no = ?`
	args := []interface{}{orderNo}
	if userID > 0 {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	order, err := scanOrder(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// ListOrders 按用户及递减 ID 游标分页，避免深 OFFSET 扫描，并限制单次返回量。
func (s *Store) ListOrders(ctx context.Context, userID int64, limit int, beforeID int64) ([]model.Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT id, order_no, user_id, activity_id, item_id, quantity, amount_cents,
	                 idempotency_key, payload_hash, reservation_id, status, expire_at,
	                 paid_at, cancelled_at, created_at
	            FROM orders WHERE user_id = ?`
	args := []interface{}{userID}
	if beforeID > 0 {
		query += " AND id < ?"
		args = append(args, beforeID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()
	orders := make([]model.Order, 0)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, *order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orders: %w", err)
	}
	return orders, nil
}

// HandleReservation 将已预扣预约落为订单，或持久化确定拒绝/多余预约的补偿依据。
// 用户和幂等键确定购买意图，预约 ID 区分同一消息重投与额外预扣；两者不能混用。
// 本方法只决定 MySQL 事实，实际 Redis 补偿与 ACK 由 Worker 在提交成功后执行。
func (s *Store) HandleReservation(ctx context.Context, in ReservationInput) (ReservationOutcome, error) {
	var out ReservationOutcome
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// 已拒绝/多余的预约不得重新建单；重投只恢复补偿进度，DONE 则不再补偿。
		// 此普通读在 RR 下可能先建立旧快照，是后续订单读取的已知并发风险起点。
		existing, err := getReservationResultTx(ctx, tx, in.ReservationID)
		if err != nil {
			return err
		}
		if existing != nil {
			out = outcomeFromReservation(existing)
			return nil
		}

		// 业务唯一键负责建行及串行化；重复时不覆盖原指纹、胜出预约或终态。
		// 请求行锁保持到整个事务结束，建单与确定拒绝均在同一锁保护下决定。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO request_results
				(user_id, idempotency_key, payload_hash, winning_reservation_id, state)
			VALUES (?, ?, ?, ?, 'PROCESSING')
			ON DUPLICATE KEY UPDATE id = id`,
			in.UserID, in.IdempotencyKey, in.PayloadHash, in.ReservationID); err != nil {
			return fmt.Errorf("insert request result: %w", err)
		}
		req, err := getRequestResultForUpdateTx(ctx, tx, in.UserID, in.IdempotencyKey)
		if err != nil {
			return err
		}

		// 已知限制：请求行是当前读，订单仍是快照读。并发 Worker 提交同键订单时，
		// 这里可能读不到新订单，随后因 SUCCEEDED 返回 ErrInconsistent，留待重投。
		order, err := getOrderByUserKeyTx(ctx, tx, in.UserID, in.IdempotencyKey)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if order != nil {
			// 已有订单是权威结果，先按它恢复请求终态，不能被迟到预约的指纹覆盖。
			if _, err := tx.ExecContext(ctx, `
				UPDATE request_results
				   SET payload_hash = ?, winning_reservation_id = ?, state = 'SUCCEEDED',
				       reason = NULL, order_no = ?
				 WHERE id = ?`,
				order.PayloadHash, order.ReservationID, order.OrderNo, req.ID); err != nil {
				return fmt.Errorf("reconcile request with order: %w", err)
			}
			if order.PayloadHash != in.PayloadHash || order.ReservationID != in.ReservationID {
				// 只终结并补偿本次多余预扣，保留原订单及业务键的成功结果。
				reason := "EXTRA_RESERVATION"
				if order.PayloadHash != in.PayloadHash {
					reason = "IDEMPOTENCY_CONFLICT"
				}
				rr, err := ensureReservationResultTx(ctx, tx, in, "EXTRA_RESERVATION", reason)
				if err != nil {
					return err
				}
				out = outcomeFromReservation(rr)
				return nil
			}
			// 同一成功预约重投无需再扣库或回补，只返回订单供 Worker 回填缓存并 ACK。
			out = ReservationOutcome{Kind: "SUCCESS", Order: order}
			return nil
		}

		// 请求终态不可逆：已拒绝的业务键不再购买，新的购买意图必须换幂等键。
		if req.State != "PROCESSING" {
			if req.State == "SUCCEEDED" {
				return ErrInconsistent
			}
			if req.State == "REJECTED" {
				outcome := "REJECTED"
				reason := req.Reason
				if req.WinningReservationID != in.ReservationID {
					outcome = "EXTRA_RESERVATION"
					reason = "EXTRA_RESERVATION"
				}
				rr, err := ensureReservationResultTx(ctx, tx, in, outcome, reason)
				if err != nil {
					return err
				}
				out = outcomeFromReservation(rr)
				return nil
			}
			return ErrInconsistent
		}
		// 未终结请求已属于另一预约时，当前无法安全判定应建单还是回补，返回异常。
		if req.WinningReservationID != "" && req.WinningReservationID != in.ReservationID {
			return ErrInconsistent
		}
		if req.PayloadHash != in.PayloadHash {
			// 指纹冲突只形成当前预约的补偿记录，不改写原购买意图。
			rr, err := ensureReservationResultTx(ctx, tx, in, "EXTRA_RESERVATION", "IDEMPOTENCY_CONFLICT")
			if err != nil {
				return err
			}
			out = outcomeFromReservation(rr)
			return nil
		}

		// 商品不存在或单次数量超限属于确定拒绝，可以先落拒绝终态再让 Worker 补偿。
		// 查询失败属于依赖异常，直接返回错误，不能把“查失败”解释为“商品不存在”。
		item, err := getItemTx(ctx, tx, in.ItemID)
		if errors.Is(err, ErrItemNotFound) {
			rr, err := rejectReservationTx(ctx, tx, in, "ITEM_NOT_FOUND")
			if err != nil {
				return err
			}
			out = outcomeFromReservation(rr)
			return nil
		}
		if err != nil {
			return err
		}
		if in.Quantity > item.PerUserLimit {
			rr, err := rejectReservationTx(ctx, tx, in, "LIMIT_EXCEEDED")
			if err != nil {
				return err
			}
			out = outcomeFromReservation(rr)
			return nil
		}
		// 保存点之前已取得请求行锁；拒绝时仅撤销后面的库存/限购变更，
		// 保留请求行及其锁，继续写拒绝终态，避免释放锁后与其他 Worker 竞争建单。
		if _, err := tx.ExecContext(ctx, "SAVEPOINT before_stock"); err != nil {
			return fmt.Errorf("savepoint: %w", err)
		}
		// 最终防超卖依靠库存条件 UPDATE 及 InnoDB 写锁，不依赖前面快照里的库存值。
		// version 仅递增记录变化，WHERE 没有比较旧 version，因此这里不是 version CAS。
		res, err := tx.ExecContext(ctx, `
			UPDATE seckill_items
			   SET available_stock = available_stock - ?, version = version + 1
			 WHERE id = ? AND available_stock >= ?`, in.Quantity, in.ItemID, in.Quantity)
		if err != nil {
			return fmt.Errorf("deduct stock: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("deduct stock rows: %w", err)
		}
		if affected == 0 {
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_stock"); err != nil {
				return fmt.Errorf("rollback stock: %w", err)
			}
			rr, rerr := rejectReservationTx(ctx, tx, in, "SOLD_OUT")
			if rerr != nil {
				return rerr
			}
			out = outcomeFromReservation(rr)
			return nil
		}

		// 限购按用户+商品累计，防止换幂等键绕过；失败时连同刚才的扣库存一起回滚。
		ok, err := applyPurchaseLimit(ctx, tx, in.ItemID, in.UserID, in.Quantity, item.PerUserLimit)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_stock"); err != nil {
				return fmt.Errorf("rollback limit: %w", err)
			}
			rr, rerr := rejectReservationTx(ctx, tx, in, "LIMIT_EXCEEDED")
			if rerr != nil {
				return rerr
			}
			out = outcomeFromReservation(rr)
			return nil
		}

		// 订单、扣库存流水和请求 SUCCEEDED 与前面的库存/限购写入共用同一事务。
		// 任一步写入失败都不返回成功订单；支付期限从此处创建订单时开始计算。
		amount := item.PriceCents * int64(in.Quantity)
		orderNo, err := newOrderNo()
		if err != nil {
			return err
		}
		expireAt := time.Now().UTC().Add(s.expireAfter)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO orders
				(order_no, user_id, activity_id, item_id, quantity, amount_cents,
				 idempotency_key, payload_hash, reservation_id, status, expire_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'PENDING_PAYMENT', ?)`,
			orderNo, in.UserID, item.ActivityID, item.ID, in.Quantity, amount,
			in.IdempotencyKey, in.PayloadHash, in.ReservationID, expireAt)
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stock_logs (order_no, item_id, delta, type)
			VALUES (?, ?, ?, 'DEDUCT')`, orderNo, item.ID, -in.Quantity); err != nil {
			return fmt.Errorf("insert deduct stock log: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE request_results
			   SET state = 'SUCCEEDED', reason = NULL, order_no = ?
			 WHERE id = ?`, orderNo, req.ID); err != nil {
			return fmt.Errorf("mark request succeeded: %w", err)
		}
		out = ReservationOutcome{
			Kind: "SUCCESS",
			Order: &model.Order{
				OrderNo:        orderNo,
				UserID:         in.UserID,
				ActivityID:     item.ActivityID,
				ItemID:         item.ID,
				Quantity:       in.Quantity,
				AmountCents:    amount,
				IdempotencyKey: in.IdempotencyKey,
				PayloadHash:    in.PayloadHash,
				ReservationID:  in.ReservationID,
				Status:         "PENDING_PAYMENT",
				ExpireAt:       expireAt,
				CreatedAt:      time.Now().UTC(),
			},
		}
		return nil
	})
	if err != nil {
		// 丢弃事务内暂存的 out；尤其提交结果不明时，Worker 必须保留消息且不盲目补偿。
		return ReservationOutcome{}, err
	}
	return out, nil
}

// MarkCompensationDone 由 Worker 在补偿 Lua 返回 OK/ALREADY 后调用，持久记录补偿完成。
// 此写入失败时应保留 Pending；重投需靠 Redis 补偿标记去重，不能再次增加库存。
// 当前仅检查 SQL 错误，未校验影响行数；缺失记录也会返回 nil，不能视为强校验接口。
func (s *Store) MarkCompensationDone(ctx context.Context, reservationID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE reservation_results
		   SET compensation_state = 'DONE', updated_at = CURRENT_TIMESTAMP(3)
		 WHERE reservation_id = ? AND compensation_state = 'PENDING'`, reservationID)
	if err != nil {
		return fmt.Errorf("mark compensation done: %w", err)
	}
	return nil
}

// MarkPaid 将模拟支付流水与订单 PAID 状态一起提交，订单行锁与超时取消互斥。
// 这里只校验金额和订单状态，不对接真实支付、退款或支付对账。
func (s *Store) MarkPaid(ctx context.Context, userID int64, orderNo, providerTxnID string, amount int64) (*model.Order, error) {
	var out *model.Order
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		order, err := getOrderByNoTx(ctx, tx, orderNo, userID)
		if err != nil {
			return err
		}
		if order.AmountCents != amount {
			return ErrPaymentAmount
		}
		if order.Status == "CANCELLED" {
			return ErrPayAfterCancel
		}
		if order.Status == "PAID" {
			// 已支付订单直接回放；此分支不再核验新传入的渠道流水号是否与原流水一致。
			out = order
			return nil
		}
		// 联合唯一键只防止同订单同流水重复，不限制同一渠道流水用于不同订单。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO payments (order_no, provider_txn_id, amount_cents, status)
			VALUES (?, ?, ?, 'SUCCESS')
			ON DUPLICATE KEY UPDATE id = id`, orderNo, providerTxnID, amount); err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		// 只允许待支付变为已支付，已取消订单不重新打开；更新失败则整个支付事务回滚。
		res, err := tx.ExecContext(ctx, `
			UPDATE orders
			   SET status = 'PAID', paid_at = CURRENT_TIMESTAMP(3)
			 WHERE order_no = ? AND user_id = ? AND status = 'PENDING_PAYMENT'`,
			orderNo, userID)
		if err != nil {
			return fmt.Errorf("mark paid: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			latest, err := getOrderByNoTx(ctx, tx, orderNo, userID)
			if err != nil {
				return err
			}
			if latest.Status == "PAID" {
				out = latest
				return nil
			}
			if latest.Status == "CANCELLED" {
				return ErrPayAfterCancel
			}
			return ErrInconsistent
		}
		latest, err := getOrderByNoTx(ctx, tx, orderNo, userID)
		if err != nil {
			return err
		}
		out = latest
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var ErrPaymentAmount = errors.New("payment amount mismatch")

// CancelExpired 只取消已落库且过期的待支付订单，不处理 Redis 中无订单的 PROCESSING。
// 当前实现整批共用一个事务（并非 SPEC 所写的单订单事务）；任一候选出错会回滚整批。
// 成功返回后 service 层才尝试 Redis 回补，两者不是原子提交，失败可能造成 Redis 少卖。
func (s *Store) CancelExpired(ctx context.Context, limit int) ([]model.CancelledOrder, error) {
	if limit <= 0 {
		limit = 100
	}
	var cancelled []model.CancelledOrder
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// 按过期索引取有界批次并锁定订单，跳过被支付/其他取消事务占用的行。
		rows, err := tx.QueryContext(ctx, `
			SELECT id, order_no, user_id, item_id, quantity
			  FROM orders
			 WHERE status = 'PENDING_PAYMENT' AND expire_at <= CURRENT_TIMESTAMP(3)
			 ORDER BY expire_at, id
			 LIMIT ? FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("select expired orders: %w", err)
		}
		type candidate struct {
			id       int64
			orderNo  string
			userID   int64
			itemID   int64
			quantity int
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.orderNo, &c.userID, &c.itemID, &c.quantity); err != nil {
				rows.Close()
				return fmt.Errorf("scan expired order: %w", err)
			}
			candidates = append(candidates, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate expired orders: %w", err)
		}
		rows.Close()

		for _, c := range candidates {
			// 先确认状态转换成功，再回补库存及限购额度；重复扫描不会再次回补已取消订单。
			res, err := tx.ExecContext(ctx, `
				UPDATE orders SET status = 'CANCELLED', cancelled_at = CURRENT_TIMESTAMP(3)
				 WHERE id = ? AND status = 'PENDING_PAYMENT'`, c.id)
			if err != nil {
				return fmt.Errorf("cancel order: %w", err)
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				continue
			}
			res, err = tx.ExecContext(ctx, `
				UPDATE seckill_items
				   SET available_stock = available_stock + ?, version = version + 1
				 WHERE id = ?`, c.quantity, c.itemID)
			if err != nil {
				return fmt.Errorf("restore item stock: %w", err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("restore item stock affected %d rows for order %s", n, c.orderNo)
			}
			// 取消释放该用户的已占限购额度；计数不足意味着持久数据异常，不继续凑平库存。
			res, err = tx.ExecContext(ctx, `
				UPDATE user_purchases SET qty = qty - ?
				 WHERE item_id = ? AND user_id = ? AND qty >= ?`,
				c.quantity, c.itemID, c.userID, c.quantity)
			if err != nil {
				return fmt.Errorf("restore purchase qty: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("purchase qty inconsistent for order %s", c.orderNo)
			}
			// CANCEL 流水与状态/库存同事务；(order_no,type) 唯一约束防重复记账。
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO stock_logs (order_no, item_id, delta, type)
				VALUES (?, ?, ?, 'CANCEL')`, c.orderNo, c.itemID, c.quantity); err != nil {
				return fmt.Errorf("insert cancel stock log: %w", err)
			}
			cancelled = append(cancelled, model.CancelledOrder{
				OrderNo: c.orderNo, ItemID: c.itemID, UserID: c.userID, Quantity: c.quantity,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cancelled, nil
}

func (s *Store) getOrderByUserKey(ctx context.Context, userID int64, key string) (*model.Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx, `
		SELECT id, order_no, user_id, activity_id, item_id, quantity, amount_cents,
		       idempotency_key, payload_hash, reservation_id, status, expire_at,
		       paid_at, cancelled_at, created_at
		  FROM orders WHERE user_id = ? AND idempotency_key = ?`, userID, key))
}

func (s *Store) getRequestResult(ctx context.Context, userID int64, key string) (*model.RequestResult, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, idempotency_key, payload_hash, winning_reservation_id,
		       state, COALESCE(reason, ''), COALESCE(order_no, ''), created_at
		  FROM request_results WHERE user_id = ? AND idempotency_key = ?`, userID, key)
	req, err := scanRequestResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return req, err
}

// getOrderByUserKeyTx 使用普通快照读；已锁请求行不意味着此查询自动变成当前读。
func getOrderByUserKeyTx(ctx context.Context, tx *sql.Tx, userID int64, key string) (*model.Order, error) {
	order, err := scanOrder(tx.QueryRowContext(ctx, `
		SELECT id, order_no, user_id, activity_id, item_id, quantity, amount_cents,
		       idempotency_key, payload_hash, reservation_id, status, expire_at,
		       paid_at, cancelled_at, created_at
		  FROM orders WHERE user_id = ? AND idempotency_key = ?`, userID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return order, err
}

func getOrderByNoTx(ctx context.Context, tx *sql.Tx, orderNo string, userID int64) (*model.Order, error) {
	// 当前读并锁住订单，使支付在等待取消事务后能看到最新终态。
	// 若用 RR 快照读，可能看见旧的待支付状态并把 PAY_AFTER_CANCEL 误报为数据异常。
	order, err := scanOrder(tx.QueryRowContext(ctx, `
		SELECT id, order_no, user_id, activity_id, item_id, quantity, amount_cents,
		       idempotency_key, payload_hash, reservation_id, status, expire_at,
		       paid_at, cancelled_at, created_at
		  FROM orders WHERE order_no = ? AND user_id = ? FOR UPDATE`, orderNo, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return order, err
}

// getRequestResultForUpdateTx 锁住购买意图并读取最新请求状态，锁由调用方事务持有。
func getRequestResultForUpdateTx(ctx context.Context, tx *sql.Tx, userID int64, key string) (*model.RequestResult, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, user_id, idempotency_key, payload_hash, winning_reservation_id,
		       state, COALESCE(reason, ''), COALESCE(order_no, ''), created_at
		  FROM request_results WHERE user_id = ? AND idempotency_key = ? FOR UPDATE`, userID, key)
	req, err := scanRequestResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInconsistent
	}
	return req, err
}

// getReservationResultTx 只查拒绝/多余预约的持久终态；没有记录不能单独证明预约未成功。
func getReservationResultTx(ctx context.Context, tx *sql.Tx, reservationID string) (*model.ReservationResult, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT reservation_id, user_id, idempotency_key, payload_hash, outcome,
		       compensation_state, COALESCE(reason, ''), attempts, COALESCE(last_error, '')
		  FROM reservation_results WHERE reservation_id = ?`, reservationID)
	rr, err := scanReservationResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rr, err
}

// ensureReservationResultTx 记录某次预扣必须补偿的事实，重复调用不重置已有 DONE 状态。
// 调用前须已决定该预约不能建单；它与业务请求终态共用事务，Redis 标记不能替代此记录。
func ensureReservationResultTx(ctx context.Context, tx *sql.Tx, in ReservationInput, outcome, reason string) (*model.ReservationResult, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reservation_results
			(reservation_id, user_id, idempotency_key, payload_hash, outcome, compensation_state, reason)
		VALUES (?, ?, ?, ?, ?, 'PENDING', ?)
		ON DUPLICATE KEY UPDATE id = id`,
		in.ReservationID, in.UserID, in.IdempotencyKey, in.PayloadHash, outcome, reason); err != nil {
		return nil, fmt.Errorf("insert reservation result: %w", err)
	}
	rr, err := getReservationResultTx(ctx, tx, in.ReservationID)
	if err != nil {
		return nil, err
	}
	if rr == nil {
		return nil, ErrInconsistent
	}
	return rr, nil
}

// outcomeFromReservation 将持久进度转为 Worker 动作；DONE 重投仅结束，不再调用回补。
func outcomeFromReservation(rr *model.ReservationResult) ReservationOutcome {
	out := ReservationOutcome{
		Kind:             rr.Outcome,
		Reason:           rr.Reason,
		CompensationDone: rr.CompensationState == "DONE",
	}
	out.Compensate = !out.CompensationDone
	return out
}

// rejectReservationTx 必须在持有业务请求行锁且业务变更已撤销的事务内调用。
// 请求拒绝与预约补偿 PENDING 一起提交，防止已回补的预约以后又被当作新预约建单。
func rejectReservationTx(ctx context.Context, tx *sql.Tx, in ReservationInput, reason string) (*model.ReservationResult, error) {
	if _, err := tx.ExecContext(ctx, `
		UPDATE request_results
		   SET state = 'REJECTED', reason = ?, order_no = NULL
		 WHERE user_id = ? AND idempotency_key = ?`,
		reason, in.UserID, in.IdempotencyKey); err != nil {
		return nil, fmt.Errorf("reject request: %w", err)
	}
	return ensureReservationResultTx(ctx, tx, in, "REJECTED", reason)
}

// getItemTx 读取价格、限购等建单依据；快照中的 available_stock 不能替代条件扣减。
func getItemTx(ctx context.Context, tx *sql.Tx, itemID int64) (*model.Item, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT i.id, i.activity_id, i.sku_code, i.name, i.price_cents, i.total_stock,
		       i.available_stock, i.per_user_limit, i.version,
		       a.start_at, a.end_at, a.status
		  FROM seckill_items i
		  JOIN activities a ON a.id = i.activity_id
		 WHERE i.id = ?`, itemID)
	item, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrItemNotFound
	}
	return item, err
}

func applyPurchaseLimit(ctx context.Context, tx *sql.Tx, itemID, userID int64, quantity, limit int) (bool, error) {
	// 先以零额度确保唯一计数行存在，再统一做带上限的 UPDATE，首次购买也不能绕过限购。
	// 不同幂等键仍竞争同一个用户+商品计数行；写锁与条件共同保护累计购买数量。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_purchases (item_id, user_id, qty)
		VALUES (?, ?, 0)
		ON DUPLICATE KEY UPDATE id = id`, itemID, userID); err != nil {
		return false, fmt.Errorf("ensure purchase row: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE user_purchases SET qty = qty + ?
		 WHERE item_id = ? AND user_id = ? AND qty + ? <= ?`,
		quantity, itemID, userID, quantity, limit)
	if err != nil {
		return false, fmt.Errorf("update purchase limit: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	return false, nil
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanItem(row rowScanner) (*model.Item, error) {
	var item model.Item
	if err := row.Scan(
		&item.ID, &item.ActivityID, &item.SKUCode, &item.Name, &item.PriceCents,
		&item.TotalStock, &item.AvailableStock, &item.PerUserLimit, &item.Version,
		&item.ActivityStart, &item.ActivityEnd, &item.ActivityStatus,
	); err != nil {
		return nil, err
	}
	return &item, nil
}

func scanOrder(row rowScanner) (*model.Order, error) {
	var order model.Order
	var paidAt, cancelledAt sql.NullTime
	if err := row.Scan(
		&order.ID, &order.OrderNo, &order.UserID, &order.ActivityID, &order.ItemID,
		&order.Quantity, &order.AmountCents, &order.IdempotencyKey, &order.PayloadHash,
		&order.ReservationID, &order.Status, &order.ExpireAt, &paidAt, &cancelledAt,
		&order.CreatedAt,
	); err != nil {
		return nil, err
	}
	if paidAt.Valid {
		t := paidAt.Time
		order.PaidAt = &t
	}
	if cancelledAt.Valid {
		t := cancelledAt.Time
		order.CancelledAt = &t
	}
	return &order, nil
}

func scanRequestResult(row rowScanner) (*model.RequestResult, error) {
	var req model.RequestResult
	if err := row.Scan(
		&req.ID, &req.UserID, &req.IdempotencyKey, &req.PayloadHash, &req.WinningReservationID,
		&req.State, &req.Reason, &req.OrderNo, &req.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &req, nil
}

func scanReservationResult(row rowScanner) (*model.ReservationResult, error) {
	var rr model.ReservationResult
	if err := row.Scan(
		&rr.ReservationID, &rr.UserID, &rr.IdempotencyKey, &rr.PayloadHash, &rr.Outcome,
		&rr.CompensationState, &rr.Reason, &rr.Attempts, &rr.LastError,
	); err != nil {
		return nil, err
	}
	return &rr, nil
}

func newOrderNo() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate order no: %w", err)
	}
	return fmt.Sprintf("SK%s%08X", time.Now().UTC().Format("20060102150405"), binary.BigEndian.Uint32(b[:])), nil
}

func (s *Store) CountOrdersByItem(ctx context.Context, itemID int64) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE item_id = ?`, itemID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count orders: %w", err)
	}
	return n, nil
}

func (s *Store) CountOrdersByUserItem(ctx context.Context, userID, itemID int64) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE user_id = ? AND item_id = ?`, userID, itemID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count user orders: %w", err)
	}
	return n, nil
}

func (s *Store) CountStockLogs(ctx context.Context, orderNo, logType string) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM stock_logs WHERE order_no = ? AND type = ?`, orderNo, logType).Scan(&n); err != nil {
		return 0, fmt.Errorf("count stock logs: %w", err)
	}
	return n, nil
}

func (s *Store) GetReservationResult(ctx context.Context, reservationID string) (*model.ReservationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reservation lookup: %w", err)
	}
	defer tx.Rollback()
	rr, err := getReservationResultTx(ctx, tx, reservationID)
	if err != nil {
		return nil, err
	}
	if rr == nil {
		return nil, ErrNotFound
	}
	return rr, tx.Commit()
}
