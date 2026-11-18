package model

import "time"

type ActivityInput struct {
	Name    string
	StartAt time.Time
	EndAt   time.Time
	Items   []ItemInput
}

type ItemInput struct {
	SKUCode      string
	Name         string
	PriceCents   int64
	TotalStock   int
	PerUserLimit int
}

type Activity struct {
	ID        int64     `json:"activityId"`
	Name      string    `json:"name"`
	StartAt   time.Time `json:"startAt"`
	EndAt     time.Time `json:"endAt"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

type Item struct {
	ID             int64     `json:"itemId"`
	ActivityID     int64     `json:"activityId"`
	SKUCode        string    `json:"skuCode"`
	Name           string    `json:"name"`
	PriceCents     int64     `json:"priceCents"`
	TotalStock     int       `json:"totalStock"`
	AvailableStock int       `json:"availableStock"`
	PerUserLimit   int       `json:"perUserLimit"`
	Version        int64     `json:"version"`
	ActivityStart  time.Time `json:"activityStartAt"`
	ActivityEnd    time.Time `json:"activityEndAt"`
	ActivityStatus string    `json:"activityStatus"`
}

type Order struct {
	ID             int64      `json:"-"`
	OrderNo        string     `json:"orderNo"`
	UserID         int64      `json:"userId"`
	ActivityID     int64      `json:"activityId"`
	ItemID         int64      `json:"itemId"`
	Quantity       int        `json:"quantity"`
	AmountCents    int64      `json:"amountCents"`
	IdempotencyKey string     `json:"-"`
	PayloadHash    string     `json:"-"`
	ReservationID  string     `json:"-"`
	Status         string     `json:"status"`
	ExpireAt       time.Time  `json:"expireAt"`
	PaidAt         *time.Time `json:"paidAt,omitempty"`
	CancelledAt    *time.Time `json:"cancelledAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
}

type RequestResult struct {
	ID                   int64     `json:"-"`
	UserID               int64     `json:"userId"`
	IdempotencyKey       string    `json:"idempotencyKey"`
	PayloadHash          string    `json:"-"`
	WinningReservationID string    `json:"reservationId,omitempty"`
	State                string    `json:"state"`
	Reason               string    `json:"reason,omitempty"`
	OrderNo              string    `json:"orderNo,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
}

type ReservationResult struct {
	ReservationID     string `json:"reservationId"`
	UserID            int64  `json:"userId"`
	IdempotencyKey    string `json:"idempotencyKey"`
	PayloadHash       string `json:"-"`
	Outcome           string `json:"outcome"`
	CompensationState string `json:"compensationState"`
	Reason            string `json:"reason,omitempty"`
	Attempts          int    `json:"attempts"`
	LastError         string `json:"lastError,omitempty"`
}

type RequestStatus struct {
	State   string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	OrderNo string `json:"orderNo,omitempty"`
	Order   *Order `json:"order,omitempty"`
}

type CancelledOrder struct {
	OrderNo  string
	ItemID   int64
	UserID   int64
	Quantity int
}
