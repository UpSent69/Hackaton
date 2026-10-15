// Package contract описывает структуры запросов и ответов, которых ждёт фронтенд.
//
// Это договор между фронтом и бэком: JSON-имена полей, типы и единицы менять нельзя
// без правки фронта. Маршруты, коды ответов и правила — в API.md рядом.
//
// Общие правила для всех структур:
//   - количества — целые числа в базовых единицах: граммы, миллилитры, штуки;
//   - время — RFC 3339 в UTC (time.Time сериализуется так по умолчанию);
//   - пустой список отдаётся как [], а не null: срезы нужно создавать через make
//     или присваивать пустой литерал, иначе encoding/json отдаст null и фронт упадёт;
//   - поля с типом-указателем могут быть null, остальные присутствуют всегда.
package contract

import "time"

// ---------- Справочные значения ----------

// Unit — базовая единица измерения продукта.
type Unit string

const (
	UnitGram       Unit = "g"
	UnitMilliliter Unit = "ml"
	UnitPiece      Unit = "pcs"
)

// ExpiryStatus — статус партии по сроку годности.
// warning: осталось меньше 30 % срока или меньше 6 часов;
// critical: меньше 10 % срока или меньше 2 часов; expired: срок вышел.
type ExpiryStatus string

const (
	StatusOK       ExpiryStatus = "ok"
	StatusWarning  ExpiryStatus = "warning"
	StatusCritical ExpiryStatus = "critical"
	StatusExpired  ExpiryStatus = "expired"
)

// Role — роль пользователя. Изменять данные может только admin.
type Role string

const (
	RoleOwner    Role = "owner"    // владелец сети: входит в любой из трёх ресторанов с полными правами
	RoleDirector Role = "director" // директор: полные права, но только в своём ресторане
	RoleManager  Role = "manager"  // менеджер: видит данные своего ресторана, рассматривает поставки
	RoleWaiter   Role = "waiter"   // официант: только страница «Заказы» — меню и оформление заказов
	RoleSupplier Role = "supplier" // поставщик: только страница «Поставки» — приёмка и импорт
)

// ---------- Ошибки ----------

// Error — тело любого ответа с кодом 4xx/5xx. Message показывается пользователю как есть,
// поэтому пишется по-русски и понятно. Shortages заполняется только при отказе в заказе (409).
type Error struct {
	Message   string     `json:"message"`
	Shortages []Shortage `json:"shortages,omitempty"`
}

// ---------- Вход и пользователи ----------

type RegisterRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"` // не короче 6 символов
}

type LoginRequest struct {
	Email    string `json:"email"` // почта администратора или логин менеджера
	Password string `json:"password"`
}

// CreateManagerRequest — администратор заводит менеджера: логин и пароль.
type CreateManagerRequest struct {
	Login    string `json:"login"`    // латиница, цифры, «.», «_», «-», от 3 до 64 символов
	Password string `json:"password"` // не короче 6 символов
	Role     Role   `json:"role,omitempty"` // director, manager (по умолчанию), waiter или supplier
	// Ресторан сотрудника: обязателен для директора, менеджера и официанта, если создаёт владелец
	// (директор всегда создаёт сотрудников своего ресторана); у поставщика не указывается.
	RestaurantID string `json:"restaurantId,omitempty"`
}

// RegistrationState — открыта ли самостоятельная регистрация (только для первых двух администраторов).
type RegistrationState struct {
	Open bool `json:"open"`
}

// Session — ответ на регистрацию и вход. Token фронт присылает в заголовке
// Authorization: Bearer <token> во всех остальных запросах.
type Session struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  Role   `json:"role"`
	// Ресторан сотрудника. У владельца и поставщика пусто: они работают со всеми ресторанами.
	RestaurantID   string `json:"restaurantId,omitempty"`
	RestaurantName string `json:"restaurantName,omitempty"`
}

// Restaurant — ресторан сети. Ресторан запроса передаётся заголовком X-Restaurant-Id
// (его учитывают только у владельца и поставщика; остальные всегда работают в своём).
type Restaurant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type SetRoleRequest struct {
	Role Role `json:"role"`
}

// ---------- Склад ----------

type Product struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Unit Unit   `json:"unit"`
}

// StockItem — остаток по одному продукту. В список попадают только продукты,
// у которых есть хотя бы одна партия.
type StockItem struct {
	ProductID        string       `json:"productId"`
	Name             string       `json:"name"`
	Unit             Unit         `json:"unit"`
	Available        int64        `json:"available"`        // сумма по непросроченным партиям
	Expired          int64        `json:"expired"`          // остаток в просроченных, ещё не списанных партиях
	BatchCount       int          `json:"batchCount"`       // всего партий продукта
	NearestExpiresAt *time.Time   `json:"nearestExpiresAt"` // ближайший срок среди непросроченных партий с остатком
	Status           ExpiryStatus `json:"status"`           // худший статус среди партий продукта
}

// Batch — партия: одна строка поставки.
type Batch struct {
	ID           string       `json:"id"`
	ProductID    string       `json:"productId"`
	SupplyID     string       `json:"supplyId"`
	SupplyNumber string       `json:"supplyNumber"`
	QtyInitial   int64        `json:"qtyInitial"`
	QtyRemaining int64        `json:"qtyRemaining"`
	ReceivedAt   time.Time    `json:"receivedAt"`
	ExpiresAt    time.Time    `json:"expiresAt"`
	Status       ExpiryStatus `json:"status"`
}

// ExpiryBatch — партия для экрана сроков: с названием продукта и списанным по сроку объёмом.
// В список входят партии с остатком и партии, по которым было списание по сроку.
type ExpiryBatch struct {
	Batch
	Name       string `json:"name"`
	Unit       Unit   `json:"unit"`
	WrittenOff int64  `json:"writtenOff"`
}

// ---------- Поставки ----------

type CreateSupplyRequest struct {
	Number   string            `json:"number"`             // номер поставки, уникален
	Items    []SupplyItemInput `json:"items"`              // не пустой
	ImportID string            `json:"importId,omitempty"` // id из POST /supplies/parse, если строки распознаны из файла
	// Ресторан, в который поставщик отправляет поставку. Остальные роли создают поставку
	// в ресторане запроса, поле не нужно.
	RestaurantID string `json:"restaurantId,omitempty"`
}

// SupplyItemInput — строка поставки. Продукт находится по названию без учёта регистра и «ё/е»;
// если такого нет — создаётся с указанной единицей. Срок годности партии = момент приёмки + ShelfLifeHours.
type SupplyItemInput struct {
	Name           string  `json:"name"`
	Unit           Unit    `json:"unit"`
	Quantity       int64   `json:"quantity"`       // больше нуля
	ShelfLifeHours float64 `json:"shelfLifeHours"`      // больше нуля
	LotNumber      string  `json:"lotNumber,omitempty"` // номер партии; пустой — сервер сгенерирует случайный
}

type Supply struct {
	ID        string       `json:"id"`
	Number    string       `json:"number"`
	CreatedAt time.Time    `json:"createdAt"`
	Items     []SupplyItem `json:"items"` // партии на складе (только у принятых поставок)

	Status     SupplyStatus         `json:"status"`
	CreatedBy  string               `json:"createdBy"`            // имя автора поставки
	Comment    string               `json:"comment"`              // комментарий проверяющего
	ReviewedAt *time.Time           `json:"reviewedAt,omitempty"` // когда рассмотрена
	Proposed   []SupplyProposedItem `json:"proposed"`             // позиции, отправленные поставщиком на проверку

	RestaurantID   string `json:"restaurantId"`
	RestaurantName string `json:"restaurantName"`
}

// SupplyStatus — состояние поставки. Поставка администратора сразу accepted;
// поставка поставщика pending, пока её не рассмотрят менеджер или администратор.
type SupplyStatus string

const (
	SupplyPending  SupplyStatus = "pending"
	SupplyAccepted SupplyStatus = "accepted"
	SupplyPartial  SupplyStatus = "partial"
	SupplyRejected SupplyStatus = "rejected"
)

// SupplyProposedItem — позиция поставки поставщика. Decision: pending, accepted или rejected.
type SupplyProposedItem struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Unit           Unit    `json:"unit"`
	Quantity       int64   `json:"quantity"`
	ShelfLifeHours float64 `json:"shelfLifeHours"`
	LotNumber      string  `json:"lotNumber"`
	Decision       string  `json:"decision"`
}

// ReviewSupplyRequest — решение по поставке на проверке.
// accept: на склад идут только позиции из AcceptedItemIDs (не пусто); остальные отклоняются.
// reject: поставка отменяется целиком, Comment обязателен.
type ReviewSupplyRequest struct {
	Action          string   `json:"action"` // accept | reject
	AcceptedItemIDs []string `json:"acceptedItemIds"`
	Comment         string   `json:"comment"`
}

type SupplyItem struct {
	ProductID string    `json:"productId"`
	Name      string    `json:"name"`
	Unit      Unit      `json:"unit"`
	Quantity  int64     `json:"quantity"`
	ExpiresAt time.Time `json:"expiresAt"`
	LotNumber string    `json:"lotNumber"`
}

// ---------- Меню ----------

type CreateDishRequest struct {
	Name        string            `json:"name"`        // уникально
	Ingredients []IngredientInput `json:"ingredients"` // не пустой, продукты не повторяются
}

type IngredientInput struct {
	ProductID string `json:"productId"`
	Quantity  int64  `json:"quantity"` // на одну порцию, больше нуля
}

// Dish — блюдо с расчётом доступности. Число порций ограничивает самый дефицитный ингредиент:
// AvailablePortions = min по ингредиентам (доступный остаток / количество на порцию).
type Dish struct {
	DishID            string       `json:"dishId"`
	Name              string       `json:"name"`
	Ingredients       []Ingredient `json:"ingredients"`
	AvailablePortions int64        `json:"availablePortions"`
	Stopped           bool         `json:"stopped"`      // AvailablePortions == 0
	LimitedBy         *Limit       `json:"limitedBy"`    // ингредиент, который ограничивает число порций
	StopReason        *Limit       `json:"stopReason"`   // то же, что LimitedBy, но только когда блюдо в стопе; иначе null
	StoppedSince      *time.Time   `json:"stoppedSince"` // с какого момента блюдо в стопе; иначе null
	Hot               bool         `json:"hot"`          // блюдо есть в горячих предложениях
	HotScore          int          `json:"hotScore"`     // его приоритет там, 0 если нет
	PriceKopecks      int64        `json:"priceKopecks"`    // цена порции для заявок; 0, если не задана
	PriceConfigured   bool         `json:"priceConfigured"` // задана ли цена порции
}

type Ingredient struct {
	ProductID string `json:"productId"`
	Name      string `json:"name"`
	Unit      Unit   `json:"unit"`
	Quantity  int64  `json:"quantity"` // на одну порцию
}

type Limit struct {
	ProductID   string `json:"productId"`
	ProductName string `json:"productName"`
	Unit        Unit   `json:"unit"`
	Required    int64  `json:"required"`  // нужно на одну порцию
	Available   int64  `json:"available"` // доступный остаток продукта
}

// ---------- Заказы ----------

type OrderRequest struct {
	Items []OrderItemInput `json:"items"` // не пустой
}

type OrderItemInput struct {
	DishID   string `json:"dishId"`
	Portions int64  `json:"portions"` // больше нуля
}

// ValidateResult — ответ на проверку корзины без списания.
type ValidateResult struct {
	OK        bool       `json:"ok"`
	Shortages []Shortage `json:"shortages"`
}

// Shortage — нехватка продукта. Считается по всему заказу сразу: потребность
// складывается по всем блюдам корзины.
type Shortage struct {
	ProductID   string   `json:"productId"`
	ProductName string   `json:"productName"`
	Unit        Unit     `json:"unit"`
	Required    int64    `json:"required"`
	Available   int64    `json:"available"`
	Missing     int64    `json:"missing"` // Required - Available
	Dishes      []string `json:"dishes"`  // названия блюд заказа, в которые идёт продукт
}

type OrderStatus string

const (
	OrderAccepted OrderStatus = "accepted"
	OrderRejected OrderStatus = "rejected"
)

// Order — заказ в истории. Отклонённые заказы тоже сохраняются: у них заполнен Shortages,
// у оформленных — Writeoffs.
type Order struct {
	ID        string      `json:"id"`
	Number    int64       `json:"number"` // порядковый номер, показывается пользователю
	CreatedAt time.Time   `json:"createdAt"`
	Status    OrderStatus `json:"status"`
	Items     []OrderLine `json:"items"`
	Shortages []Shortage  `json:"shortages"`
	Writeoffs []Writeoff  `json:"writeoffs"`
}

type OrderLine struct {
	DishID   string `json:"dishId"`
	Name     string `json:"name"`
	Portions int64  `json:"portions"`
}

// Writeoff — списание из одной партии при оформлении заказа. Партии выбираются
// по самому раннему сроку годности (FEFO); одна потребность может разойтись по нескольким партиям.
type Writeoff struct {
	ProductID    string    `json:"productId"`
	ProductName  string    `json:"productName"`
	Unit         Unit      `json:"unit"`
	BatchID      string    `json:"batchId"`
	SupplyNumber string    `json:"supplyNumber"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Quantity     int64     `json:"quantity"`
}

// ---------- Уведомления и состояние ----------

// AlertLevel: warning и critical — партия подходит к концу срока; expired — списана по сроку;
// stop — блюдо попало в стоп-лист; ok — блюдо снова доступно.
type AlertLevel string

type Alert struct {
	ID    string     `json:"id"`
	At    time.Time  `json:"at"`
	Level AlertLevel `json:"level"`
	Text  string     `json:"text"` // готовый текст по-русски
}

// Pulse — лёгкий ответ для опроса раз в три секунды.
// Rev растёт при каждом изменении данных, включая списание по сроку и смену статуса партии:
// по нему страница понимает, что пора перезапросить свои данные.
type Pulse struct {
	Rev    int64     `json:"rev"`
	Now    time.Time `json:"now"`    // текущее время сервера с учётом демо-ускорения
	Speed  float64   `json:"speed"`  // 1 — реальное время
	Alerts []Alert   `json:"alerts"` // пять последних, новые первыми
}

// ---------- Горячие предложения ----------

// HotOffer — блюдо, продажу которого стоит стимулировать. Список отсортирован по Score по убыванию;
// блюда в стопе в него не входят.
type HotOffer struct {
	// ID — «auto-…» у рассчитанных предложений, «manual-N» у добавленных вручную.
	ID     string `json:"id"`
	Source string `json:"source"` // auto | manual
	// Status у ручных: pending (ждёт менеджера), active, rejected; у автоматических всегда active.
	Status string `json:"status"`
	// Hidden — скрыто менеджером: официант такое предложение не видит, остальные — полупрозрачным.
	Hidden    bool   `json:"hidden"`
	Note      string `json:"note,omitempty"`      // причина ручного предложения
	CreatedBy string `json:"createdBy,omitempty"` // кто добавил ручное предложение

	DishID            string      `json:"dishId"`
	Name              string      `json:"name"`
	Score             int         `json:"score"` // приоритет 0…100
	AvailablePortions int64       `json:"availablePortions"`
	Target            HotTarget   `json:"target"`
	Reasons           []HotReason `json:"reasons"` // по убыванию Weight, первая — главная
}

// ChatMessage — сообщение общего чата сети. Role — должность автора (на фронте — первая буква),
// Restaurant — его ресторан или «Вся сеть» у владельца и поставщика.
type ChatMessage struct {
	ID         string    `json:"id"`
	Author     string    `json:"author"`
	Role       Role      `json:"role"`
	Restaurant string    `json:"restaurant"`
	Text       string    `json:"text"`
	At         time.Time `json:"at"`
	Mine       bool      `json:"mine"`
	CanDelete  bool      `json:"canDelete"` // своё сообщение или пользователь — владелец
}

// CreateHotOfferRequest — ручное горячее предложение. Официант создаёт его на рассмотрение
// менеджеру; менеджер, директор и владелец — сразу действующим.
type CreateHotOfferRequest struct {
	DishID   string    `json:"dishId"`
	Reason   string    `json:"reason"`   // до 300 символов
	Portions int64     `json:"portions"` // цель: сколько порций продать, 1…10000
	Until    time.Time `json:"until"`    // до какого времени действует (не дальше 7 суток)
}

type HotTarget struct {
	Portions int64     `json:"portions"` // сколько порций продать
	Until    time.Time `json:"until"`    // до какого времени
}

// HotReason — один продукт, из-за которого блюдо попало в список. Из этих чисел фронт собирает объяснение.
type HotReason struct {
	Weight          float64   `json:"weight"` // срочность × покрытие × эксклюзивность
	ProductID       string    `json:"productId"`
	ProductName     string    `json:"productName"`
	Unit            Unit      `json:"unit"`
	SupplyNumber    string    `json:"supplyNumber"`
	ExpiresAt       time.Time `json:"expiresAt"`
	BatchQuantity   int64     `json:"batchQuantity"`   // остаток партии
	AtRisk          int64     `json:"atRisk"`          // сколько не успеет уйти до конца срока при текущих продажах
	PerPortion      int64     `json:"perPortion"`      // расход на порцию этого блюда
	PortionsToClear int64     `json:"portionsToClear"` // сколько порций закроют AtRisk (не больше доступных)
	OtherDishes     int       `json:"otherDishes"`     // в скольких ещё доступных блюдах используется продукт
}

// ---------- Аналитика ----------

type Analytics struct {
	Orders         OrderTotals `json:"orders"`
	Writeoffs      []Writeoffs `json:"writeoffs"`      // по убыванию Share
	WriteoffEvents int         `json:"writeoffEvents"` // число списаний по сроку
	Deficits       []Deficit   `json:"deficits"`       // по убыванию Rejections
	Forecast       []Forecast  `json:"forecast"`       // по возрастанию Hours
	Purchases      []Purchase  `json:"purchases"`      // самые срочные первыми
}

type OrderTotals struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// Writeoffs — списано по сроку по одному продукту.
type Writeoffs struct {
	ProductID string  `json:"productId"`
	Name      string  `json:"name"`
	Unit      Unit    `json:"unit"`
	Quantity  int64   `json:"quantity"` // списано по сроку
	Received  int64   `json:"received"` // поступило всего
	Share     float64 `json:"share"`    // Quantity / Received, 0…1
}

// Deficit — продукт, из-за которого отклоняли заказы.
type Deficit struct {
	ProductID  string `json:"productId"`
	Name       string `json:"name"`
	Unit       Unit   `json:"unit"`
	Rejections int    `json:"rejections"` // в скольких отклонённых заказах не хватило
	Missing    int64  `json:"missing"`    // сколько не хватило суммарно
}

// ForecastCause: consumption — закончится при текущих продажах; expiry — истечёт срок годности;
// now — блюдо уже в стопе.
type ForecastCause string

// Forecast — когда блюдо попадёт в стоп-лист.
type Forecast struct {
	DishID      string        `json:"dishId"`
	Name        string        `json:"name"`
	Stopped     bool          `json:"stopped"`
	Hours       float64       `json:"hours"` // через сколько часов; 0, если уже в стопе
	Cause       ForecastCause `json:"cause"`
	ProductName string        `json:"productName"` // продукт, из-за которого это случится
	At          time.Time     `json:"at"`          // тот же момент абсолютным временем
}

// PurchaseBasis: rate — рекомендация по фактическому расходу; recipes — истории продаж нет,
// расчёт по составу блюд.
type PurchaseBasis string

// Purchase — рекомендация закупки.
type Purchase struct {
	ProductID string        `json:"productId"`
	Name      string        `json:"name"`
	Unit      Unit          `json:"unit"`
	Usable    int64         `json:"usable"`    // доступный остаток
	HoursLeft *float64      `json:"hoursLeft"` // на сколько часов хватит; null при Basis == "recipes"
	PerDay    float64       `json:"perDay"`    // расход в сутки; 0 при Basis == "recipes"
	Quantity  int64         `json:"quantity"`  // сколько стоит заказать
	Basis     PurchaseBasis `json:"basis"`
}

// ---------- Демо-режим ----------

// DemoState — состояние часов сервера.
type DemoState struct {
	Now     time.Time `json:"now"`
	Speed   float64   `json:"speed"`   // 1, 60, 600 или 3600
	Shifted bool      `json:"shifted"` // время отличается от реального (ускорено или перемотано)
}

type ClockRequest struct {
	Speed float64 `json:"speed"` // 1, 60, 600 или 3600
}

type AdvanceRequest struct {
	Hours float64 `json:"hours"` // от 1 до 240
}
