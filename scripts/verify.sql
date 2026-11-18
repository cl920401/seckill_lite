-- Replace the item id with the item created for the run.
SET @item_id = 1;

SELECT id, total_stock, available_stock,
       (SELECT COUNT(*) FROM orders o WHERE o.item_id = s.id) AS order_count
  FROM seckill_items s
 WHERE id = @item_id;

SELECT 'negative_stock' AS check_name, COUNT(*) AS failures
  FROM seckill_items WHERE available_stock > total_stock OR available_stock < 0;

SELECT 'duplicate_user_business_key' AS check_name, COUNT(*) AS failures
  FROM (
    SELECT user_id, idempotency_key, COUNT(*) AS n
      FROM orders
     GROUP BY user_id, idempotency_key
    HAVING n > 1
  ) d;

SELECT 'duplicate_order_no' AS check_name, COUNT(*) AS failures
  FROM (
    SELECT order_no, COUNT(*) AS n FROM orders GROUP BY order_no HAVING n > 1
  ) d;

SELECT 'duplicate_cancel_log' AS check_name, COUNT(*) AS failures
  FROM (
    SELECT order_no, COUNT(*) AS n FROM stock_logs
     WHERE type = 'CANCEL' GROUP BY order_no HAVING n > 1
  ) d;
