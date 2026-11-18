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

type ReservationInput struct {
	ReservationID  string
	UserID         int64
	IdempotencyKey string
	ItemID         int64
	Quantity       int
	PayloadHash    string
}

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

func (s *Store) HandleReservation(ctx context.Context, in ReservationInput) (ReservationOutcome, error) {
	var out ReservationOutcome
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		existing, err := getReservationResultTx(ctx, tx, in.ReservationID)
		if err != nil {
			return err
		}
		if existing != nil {
			out = outcomeFromReservation(existing)
			return nil
		}

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

		order, err := getOrderByUserKeyTx(ctx, tx, in.UserID, in.IdempotencyKey)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if order != nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE request_results
				   SET payload_hash = ?, winning_reservation_id = ?, state = 'SUCCEEDED',
				       reason = NULL, order_no = ?
				 WHERE id = ?`,
				order.PayloadHash, order.ReservationID, order.OrderNo, req.ID); err != nil {
				return fmt.Errorf("reconcile request with order: %w", err)
			}
			if order.PayloadHash != in.PayloadHash || order.ReservationID != in.ReservationID {
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
			out = ReservationOutcome{Kind: "SUCCESS", Order: order}
			return nil
		}

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
		if req.WinningReservationID != "" && req.WinningReservationID != in.ReservationID {
			return ErrInconsistent
		}
		if req.PayloadHash != in.PayloadHash {
			rr, err := ensureReservationResultTx(ctx, tx, in, "EXTRA_RESERVATION", "IDEMPOTENCY_CONFLICT")
			if err != nil {
				return err
			}
			out = outcomeFromReservation(rr)
			return nil
		}

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
		if _, err := tx.ExecContext(ctx, "SAVEPOINT before_stock"); err != nil {
			return fmt.Errorf("savepoint: %w", err)
		}
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
		return ReservationOutcome{}, err
	}
	return out, nil
}

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
			out = order
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO payments (order_no, provider_txn_id, amount_cents, status)
			VALUES (?, ?, ?, 'SUCCESS')
			ON DUPLICATE KEY UPDATE id = id`, orderNo, providerTxnID, amount); err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
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

func (s *Store) CancelExpired(ctx context.Context, limit int) ([]model.CancelledOrder, error) {
	if limit <= 0 {
		limit = 100
	}
	var cancelled []model.CancelledOrder
	err := s.withTx(ctx, func(tx *sql.Tx) error {
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
	// Lock the order row so payment sees the latest committed status when it
	// races with cancellation; a plain snapshot read can be stale in
	// REPEATABLE READ and misclassify PAY_AFTER_CANCEL as inconsistent state.
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

func outcomeFromReservation(rr *model.ReservationResult) ReservationOutcome {
	out := ReservationOutcome{
		Kind:             rr.Outcome,
		Reason:           rr.Reason,
		CompensationDone: rr.CompensationState == "DONE",
	}
	out.Compensate = !out.CompensationDone
	return out
}

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
	// Ensure the unique purchase row exists with qty=0 before applying the
	// guarded update. Never insert the requested quantity directly.
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
