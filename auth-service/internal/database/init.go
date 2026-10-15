package database

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Open подключается к общей базе без миграций — для ai-service и notification-service:
// схему создаёт и обновляет только core (Init), чтобы сервисы не мигрировали её одновременно.
func Open(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	return db, db.Ping()
}

// Init prepares the SQLite schema. Existing legacy auth tables are kept so an old
// local database can be migrated without losing its administrator accounts.
func Init(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec("PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect sqlite database: %w", err)
	}

	schema := "CREATE TABLE IF NOT EXISTS admins (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT UNIQUE NOT NULL, password TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS users (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, login TEXT UNIQUE NOT NULL, password TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS accounts (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE COLLATE NOCASE, " +
		"password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','manager','waiter','supplier')), created_at TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS auth_sessions (" +
		"jti TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, " +
		"expires_at TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);" +
		"CREATE TABLE IF NOT EXISTS products (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, norm_name TEXT NOT NULL UNIQUE, " +
		"unit TEXT NOT NULL CHECK(unit IN ('g','ml','pcs')), base_price_kopecks INTEGER NULL);" +
		"CREATE TABLE IF NOT EXISTS supplies (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, number TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS batches (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, product_id INTEGER NOT NULL REFERENCES products(id), " +
		"supply_id INTEGER NOT NULL REFERENCES supplies(id) ON DELETE CASCADE, qty_initial INTEGER NOT NULL, " +
		"qty_remaining INTEGER NOT NULL, written_off INTEGER NOT NULL DEFAULT 0, received_at TEXT NOT NULL, " +
		"expires_at TEXT NOT NULL, last_status TEXT NOT NULL DEFAULT 'ok');" +
		"CREATE INDEX IF NOT EXISTS idx_batches_product_expiry ON batches(product_id, expires_at, id);" +
		"CREATE INDEX IF NOT EXISTS idx_batches_supply ON batches(supply_id);" +
		"CREATE TABLE IF NOT EXISTS dishes (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE COLLATE NOCASE, stopped_since TEXT NULL);" +
		"CREATE TABLE IF NOT EXISTS dish_ingredients (" +
		"dish_id INTEGER NOT NULL REFERENCES dishes(id) ON DELETE CASCADE, " +
		"product_id INTEGER NOT NULL REFERENCES products(id), quantity INTEGER NOT NULL CHECK(quantity > 0), " +
		"PRIMARY KEY(dish_id, product_id));" +
		"CREATE TABLE IF NOT EXISTS orders (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, order_number INTEGER NOT NULL UNIQUE, created_at TEXT NOT NULL, " +
		"status TEXT NOT NULL CHECK(status IN ('accepted','rejected')), shortages_json TEXT NOT NULL DEFAULT '[]');" +
		"CREATE TABLE IF NOT EXISTS order_items (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, order_id INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE, " +
		"dish_id TEXT NOT NULL, name TEXT NOT NULL, portions INTEGER NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS order_writeoffs (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, order_id INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE, " +
		"product_id TEXT NOT NULL, product_name TEXT NOT NULL, unit TEXT NOT NULL, batch_id TEXT NOT NULL, " +
		"supply_number TEXT NOT NULL, expires_at TEXT NOT NULL, quantity INTEGER NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS alerts (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, level TEXT NOT NULL, text TEXT NOT NULL);" +
		"CREATE TABLE IF NOT EXISTS customer_requests (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, request_number TEXT NOT NULL UNIQUE, customer_name TEXT NOT NULL, " +
		"source_filename TEXT NOT NULL DEFAULT '', source_sha256 TEXT NOT NULL DEFAULT '', " +
		"global_discount_percent INTEGER NOT NULL CHECK(global_discount_percent BETWEEN 0 AND 100), " +
		"subtotal_kopecks INTEGER NOT NULL DEFAULT 0, total_kopecks INTEGER NOT NULL DEFAULT 0, " +
		"created_at TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'saved');" +
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_customer_requests_source_hash ON customer_requests(source_sha256) WHERE source_sha256 <> '';" +
		requestItemsSchema +
		"CREATE TABLE IF NOT EXISTS supply_pending_items (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, supply_id INTEGER NOT NULL REFERENCES supplies(id) ON DELETE CASCADE, " +
		"name TEXT NOT NULL, unit TEXT NOT NULL, quantity INTEGER NOT NULL, shelf_life_hours REAL NOT NULL, " +
		"lot_number TEXT NOT NULL DEFAULT '', decision TEXT NOT NULL DEFAULT 'pending');" +
		"CREATE TABLE IF NOT EXISTS supply_imports (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, source_filename TEXT NOT NULL DEFAULT '', source_sha256 TEXT NOT NULL DEFAULT '', " +
		"parser_mode TEXT NOT NULL, ai_used INTEGER NOT NULL DEFAULT 0, detected_number TEXT NOT NULL DEFAULT '', " +
		"items_json TEXT NOT NULL DEFAULT '[]', warnings_json TEXT NOT NULL DEFAULT '[]', created_at TEXT NOT NULL, " +
		"supply_id INTEGER NULL REFERENCES supplies(id) ON DELETE SET NULL, saved_at TEXT NULL);" +
		"CREATE INDEX IF NOT EXISTS idx_supply_imports_hash ON supply_imports(source_sha256);" +
		"CREATE TABLE IF NOT EXISTS app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);" +
		"CREATE INDEX IF NOT EXISTS idx_orders_created ON orders(created_at DESC);" +
		"CREATE INDEX IF NOT EXISTS idx_alerts_at ON alerts(at DESC);"

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create sqlite schema: %w", err)
	}

	// Existing SQLite databases need this additive migration as products previously had no price.
	if err := ensureColumn(db, "products", "base_price_kopecks", "INTEGER NULL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate product catalogue price: %w", err)
	}
	// Цена порции блюда — по ней считаются заявки на банкеты.
	if err := ensureColumn(db, "dishes", "price_kopecks", "INTEGER NULL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate dish price: %w", err)
	}
	if err := migrateRequestItems(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate request items: %w", err)
	}
	// Batch (lot) number of a supply row; older batches simply keep an empty value.
	// Проверка поставок: статус, автор и решение проверяющего. Старые поставки считаются принятыми.
	for column, definition := range map[string]string{
		"status": "TEXT NOT NULL DEFAULT 'accepted'", "created_by": "INTEGER NULL",
		"review_comment": "TEXT NOT NULL DEFAULT ''", "reviewed_at": "TEXT NULL",
	} {
		if err := ensureColumn(db, "supplies", column, definition); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate supply review: %w", err)
		}
	}
	if err := ensureColumn(db, "batches", "lot_number", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate batch lot number: %w", err)
	}

	if err := migrateRoles(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate account roles: %w", err)
	}
	if err := migrateStaffRoles(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate staff roles: %w", err)
	}

	// Migrate administrator records created by the older application.
	// Fresh installations intentionally start without a seeded account: the first
	// successful registration becomes the administrator, as specified by API.md.
	if _, err := db.Exec(
		"INSERT OR IGNORE INTO accounts(name, email, password_hash, role, created_at) " +
			"SELECT name, lower(email), password, 'admin', ? FROM admins WHERE email IS NOT NULL AND password IS NOT NULL",
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate legacy administrators: %w", err)
	}

	if err := migrateRestaurants(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate restaurants: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(
		"INSERT OR IGNORE INTO app_meta(key,value) VALUES " +
			"('revision','1'),('clock_speed','1'),('clock_base_real',?),('clock_base_sim',?)",
		now, now,
	)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize app state: %w", err)
	}

	return db, nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

// migrateRoles переводит старую роль «viewer» в «manager». Ограничение CHECK в SQLite
// нельзя изменить на месте, поэтому таблица пересобирается; внешние ключи на время
// выключены, чтобы сессии пользователей не удалились каскадом.
// Один раз, при первом запуске новой версии, второй по порядку аккаунт тоже становится
// администратором: правило «первые два — администраторы» действует и для старых баз.
func migrateRoles(db *sql.DB) error {
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='accounts'").Scan(&schema); err != nil {
		return err
	}
	if !strings.Contains(schema, "'viewer'") {
		return nil
	}
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer db.Exec("PRAGMA foreign_keys = ON")

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"CREATE TABLE accounts_new (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE COLLATE NOCASE, " +
			"password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','manager','waiter','supplier')), created_at TEXT NOT NULL)",
		"INSERT INTO accounts_new(id,name,email,password_hash,role,created_at) " +
			"SELECT id,name,email,password_hash,CASE WHEN role='admin' THEN 'admin' ELSE 'manager' END,created_at FROM accounts",
		"DROP TABLE accounts",
		"ALTER TABLE accounts_new RENAME TO accounts",
		"UPDATE accounts SET role='admin' WHERE id IN (SELECT id FROM accounts ORDER BY id LIMIT 2)",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Позиции заявки — блюда меню с числом порций. Название и цена сохраняются снимком, поэтому
// заявка не меняется, если блюдо потом переименуют, удалят или поменяют ему цену.
const requestItemsSchema = "CREATE TABLE IF NOT EXISTS customer_request_items (" +
	"id INTEGER PRIMARY KEY AUTOINCREMENT, request_id INTEGER NOT NULL REFERENCES customer_requests(id) ON DELETE CASCADE, " +
	"dish_id INTEGER NOT NULL, dish_name TEXT NOT NULL, portions INTEGER NOT NULL CHECK(portions > 0), " +
	"unit_price_kopecks INTEGER NOT NULL CHECK(unit_price_kopecks >= 0), " +
	"item_discount_percent INTEGER NOT NULL CHECK(item_discount_percent BETWEEN 0 AND 100), " +
	"subtotal_kopecks INTEGER NOT NULL, total_kopecks INTEGER NOT NULL);" +
	"CREATE INDEX IF NOT EXISTS idx_customer_request_items_request ON customer_request_items(request_id, id);"

// migrateRequestItems переводит заявки с продуктов на блюда. Старые заявки на продукты
// удаляются (по решению команды), таблица позиций создаётся заново.
func migrateRequestItems(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(customer_request_items)")
	if err != nil {
		return err
	}
	productBased := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "product_id" {
			productBased = true
		}
	}
	rows.Close()
	if !productBased {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"DELETE FROM customer_request_items",
		"DELETE FROM customer_requests",
		"DROP TABLE customer_request_items",
		requestItemsSchema,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateStaffRoles расширяет ограничение роли в accounts ролями официанта и поставщика.
// SQLite не меняет CHECK на месте, поэтому таблица пересоздаётся; данные и id сохраняются.
func migrateStaffRoles(db *sql.DB) error {
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='accounts'").Scan(&schema); err != nil {
		return err
	}
	if strings.Contains(schema, "'waiter'") {
		return nil
	}
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer db.Exec("PRAGMA foreign_keys = ON")

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"CREATE TABLE accounts_new (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE COLLATE NOCASE, " +
			"password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','manager','waiter','supplier')), created_at TEXT NOT NULL)",
		"INSERT INTO accounts_new(id,name,email,password_hash,role,created_at) SELECT id,name,email,password_hash,role,created_at FROM accounts",
		"DROP TABLE accounts",
		"ALTER TABLE accounts_new RENAME TO accounts",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Сеть из трёх ресторанов. Данные каждого ресторана (продукты, поставки, партии, меню,
// заказы, заявки, уведомления) привязаны к нему через restaurant_id корневых таблиц;
// дочерние таблицы (партии, состав блюд, позиции заказов) наследуют ресторан через связи.
// Всё, что было в базе до разделения, отходит ресторану 1.
var restaurantNames = []string{"ресторан-1", "ресторан-2", "ресторан-3"}

func migrateRestaurants(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS restaurants (id INTEGER PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		return err
	}
	for i, name := range restaurantNames {
		if _, err := db.Exec("INSERT OR IGNORE INTO restaurants(id,name) VALUES(?,?)", i+1, name); err != nil {
			return err
		}
	}
	// Уникальность названий и номеров теперь в пределах ресторана: «Томаты» есть в каждом.
	rebuilds := []struct {
		table, column, unique string
	}{
		{"products", "norm_name TEXT NOT NULL UNIQUE", "UNIQUE(restaurant_id, norm_name)"},
		{"supplies", "number TEXT NOT NULL UNIQUE", "UNIQUE(restaurant_id, number)"},
		{"dishes", "name TEXT NOT NULL UNIQUE COLLATE NOCASE", "UNIQUE(restaurant_id, name)"},
		{"orders", "order_number INTEGER NOT NULL UNIQUE", "UNIQUE(restaurant_id, order_number)"},
	}
	for _, rebuild := range rebuilds {
		if err := addRestaurantWithUnique(db, rebuild.table, rebuild.column, rebuild.unique); err != nil {
			return fmt.Errorf("%s: %w", rebuild.table, err)
		}
	}
	for _, table := range []string{"alerts", "customer_requests", "supply_imports"} {
		if err := ensureColumn(db, table, "restaurant_id", "INTEGER NOT NULL DEFAULT 1"); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_created ON orders(created_at DESC);" +
		"CREATE INDEX IF NOT EXISTS idx_alerts_restaurant ON alerts(restaurant_id, id DESC);"); err != nil {
		return err
	}
	return migrateOwnerRoles(db)
}

var createTableName = regexp.MustCompile(`^CREATE TABLE\s+"?(\w+)"?\s*\(`)

// addRestaurantWithUnique пересоздаёт таблицу: убирает глобальную уникальность столбца
// и добавляет restaurant_id с уникальностью в пределах ресторана. Данные и id сохраняются.
func addRestaurantWithUnique(db *sql.DB, table, uniqueColumn, unique string) error {
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&schema); err != nil {
		return err
	}
	if strings.Contains(schema, "restaurant_id") {
		return nil
	}
	if !strings.Contains(schema, uniqueColumn) {
		return fmt.Errorf("неожиданная схема: %s", schema)
	}
	columns, err := tableColumns(db, table)
	if err != nil {
		return err
	}
	newSchema := strings.Replace(schema, uniqueColumn, strings.Replace(uniqueColumn, " UNIQUE", "", 1), 1)
	newSchema = createTableName.ReplaceAllString(newSchema, "CREATE TABLE "+table+"_new (")
	end := strings.LastIndex(newSchema, ")")
	newSchema = newSchema[:end] + ", restaurant_id INTEGER NOT NULL DEFAULT 1 REFERENCES restaurants(id), " + unique + ")"

	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer db.Exec("PRAGMA foreign_keys = ON")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	list := strings.Join(columns, ",")
	for _, statement := range []string{
		newSchema,
		"INSERT INTO " + table + "_new(" + list + ",restaurant_id) SELECT " + list + ",1 FROM " + table,
		"DROP TABLE " + table,
		"ALTER TABLE " + table + "_new RENAME TO " + table,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("%w (%s)", err, statement)
		}
	}
	return tx.Commit()
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]string, 0)
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var value sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &value, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// migrateOwnerRoles переводит аккаунты на роли сети: первый администратор — владелец,
// остальные администраторы — директора ресторана 1, менеджеры и официанты — сотрудники
// ресторана 1, поставщики — общие для всех ресторанов.
func migrateOwnerRoles(db *sql.DB) error {
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='accounts'").Scan(&schema); err != nil {
		return err
	}
	if strings.Contains(schema, "'owner'") {
		return nil
	}
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer db.Exec("PRAGMA foreign_keys = ON")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"CREATE TABLE accounts_new (" +
			"id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, email TEXT NOT NULL UNIQUE COLLATE NOCASE, " +
			"password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('owner','director','manager','waiter','supplier')), " +
			"created_at TEXT NOT NULL, restaurant_id INTEGER NULL REFERENCES restaurants(id))",
		"INSERT INTO accounts_new(id,name,email,password_hash,role,created_at,restaurant_id) " +
			"SELECT id,name,email,password_hash," +
			"CASE WHEN role='admin' AND id=(SELECT MIN(id) FROM accounts WHERE role='admin') THEN 'owner' " +
			"WHEN role='admin' THEN 'director' ELSE role END," +
			"created_at," +
			"CASE WHEN role='supplier' OR (role='admin' AND id=(SELECT MIN(id) FROM accounts WHERE role='admin')) THEN NULL ELSE 1 END " +
			"FROM accounts",
		"DROP TABLE accounts",
		"ALTER TABLE accounts_new RENAME TO accounts",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
