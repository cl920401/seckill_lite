-- SECKILL MVP schema (design artifact)
-- Target: MySQL 8.0.16+ / 8.4, InnoDB
-- This file is NOT executed as part of the current documentation task.

SET NAMES utf8mb4;

CREATE TABLE activities (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name        VARCHAR(128) NOT NULL,
  start_at    DATETIME(3) NOT NULL,
  end_at      DATETIME(3) NOT NULL,
  status      VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'ACTIVE',
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_activities_window (status, start_at, end_at),
  CONSTRAINT chk_activities_window CHECK (end_at > start_at),
  CONSTRAINT chk_activities_status CHECK (status IN ('ACTIVE', 'DISABLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE seckill_items (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  activity_id      BIGINT UNSIGNED NOT NULL,
  sku_code         VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  name             VARCHAR(128) NOT NULL,
  price_cents      BIGINT UNSIGNED NOT NULL,
  total_stock      INT UNSIGNED NOT NULL,
  available_stock  INT UNSIGNED NOT NULL,
  per_user_limit   INT UNSIGNED NOT NULL,
  version          BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_items_activity_sku (activity_id, sku_code),
  KEY idx_items_activity (activity_id),
  CONSTRAINT chk_items_stock CHECK (total_stock > 0 AND available_stock <= total_stock),
  CONSTRAINT chk_items_limit CHECK (per_user_limit > 0 AND per_user_limit <= total_stock)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE orders (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_no         VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id          BIGINT UNSIGNED NOT NULL,
  activity_id      BIGINT UNSIGNED NOT NULL,
  item_id          BIGINT UNSIGNED NOT NULL,
  quantity         INT UNSIGNED NOT NULL,
  amount_cents     BIGINT UNSIGNED NOT NULL,
  idempotency_key  VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  payload_hash     CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  reservation_id   VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status           VARCHAR(20) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  expire_at        DATETIME(3) NOT NULL,
  paid_at          DATETIME(3) NULL,
  cancelled_at     DATETIME(3) NULL,
  created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_orders_order_no (order_no),
  UNIQUE KEY uk_orders_user_key (user_id, idempotency_key),
  UNIQUE KEY uk_orders_reservation (reservation_id),
  KEY idx_orders_expire (status, expire_at, id),
  KEY idx_orders_user (user_id, id),
  KEY idx_orders_item_status (item_id, status),
  CONSTRAINT chk_orders_qty CHECK (quantity > 0),
  CONSTRAINT chk_orders_status CHECK (status IN ('PENDING_PAYMENT', 'PAID', 'CANCELLED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE request_results (
  id                      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id                 BIGINT UNSIGNED NOT NULL,
  idempotency_key         VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  payload_hash            CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  winning_reservation_id  VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  state                   VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  reason                  VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
  order_no                VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  created_at              DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at              DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_request_business (user_id, idempotency_key),
  KEY idx_request_order (order_no),
  KEY idx_request_reservation (winning_reservation_id),
  CONSTRAINT chk_request_state CHECK (state IN ('PROCESSING', 'SUCCEEDED', 'REJECTED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE reservation_results (
  id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  reservation_id      VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id             BIGINT UNSIGNED NOT NULL,
  idempotency_key     VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  payload_hash        CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  outcome             VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  compensation_state  VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'PENDING',
  reason              VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
  attempts            INT UNSIGNED NOT NULL DEFAULT 0,
  last_error          VARCHAR(255) NULL,
  created_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at          DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_reservation (reservation_id),
  KEY idx_reservation_business (user_id, idempotency_key),
  KEY idx_reservation_compensation (compensation_state, outcome, id),
  CONSTRAINT chk_reservation_outcome CHECK (outcome IN ('REJECTED', 'EXTRA_RESERVATION')),
  CONSTRAINT chk_reservation_comp CHECK (compensation_state IN ('PENDING', 'DONE'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE user_purchases (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  item_id     BIGINT UNSIGNED NOT NULL,
  user_id     BIGINT UNSIGNED NOT NULL,
  qty         INT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_purchase_item_user (item_id, user_id),
  CONSTRAINT chk_purchase_qty CHECK (qty >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE stock_logs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_no    VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  item_id     BIGINT UNSIGNED NOT NULL,
  delta       BIGINT NOT NULL,
  type        VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_stock_order_type (order_no, type),
  KEY idx_stock_item (item_id, created_at),
  CONSTRAINT chk_stock_type CHECK (type IN ('DEDUCT', 'CANCEL')),
  CONSTRAINT chk_stock_delta CHECK (
    (type = 'DEDUCT' AND delta < 0) OR (type = 'CANCEL' AND delta > 0)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE payments (
  id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_no         VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  provider_txn_id  VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  amount_cents     BIGINT UNSIGNED NOT NULL,
  status           VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'SUCCESS',
  created_at       DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_payment_order_txn (order_no, provider_txn_id),
  KEY idx_payment_order (order_no),
  CONSTRAINT chk_payment_status CHECK (status IN ('SUCCESS'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
