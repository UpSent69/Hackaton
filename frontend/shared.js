/* Общее для отдельных страниц (supplies.html, orders.html, stoplist.html, analytics.html):
 * шапка с навигацией, проверка входа и роли, форматирование, автообновление и мелкие анимации.
 * Данные приходят только через API (см. api.js). */
const UI = (() => {
  /* ---------- Форматирование ---------- */

  const escapeHtml = (value) =>
    String(value ?? '').replace(/[&<>"']/g, (symbol) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[symbol])

  const numberFormat = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 1 })
  const dateFormat = new Intl.DateTimeFormat('ru-RU', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' })

  const UNIT_LABEL = { g: 'г', ml: 'мл', pcs: 'шт' }

  const STATUS = {
    ok: { label: 'В норме', color: '#e9e9e9' },
    warning: { label: 'Скоро истекает', color: '#f2c24b' },
    critical: { label: 'Истекает', color: '#ff8f4d' },
    expired: { label: 'Просрочено', color: '#ff5f5f' },
  }

  // 2400 мл → «2,4 л», 600 г → «600 г», 140 шт → «140 шт»
  function formatQuantity(value, unit) {
    if (unit === 'pcs') return numberFormat.format(value) + ' шт'
    const [small, large] = unit === 'g' ? ['г', 'кг'] : ['мл', 'л']
    return value >= 1000 ? numberFormat.format(value / 1000) + ' ' + large : numberFormat.format(value) + ' ' + small
  }

  const formatDateTime = (value) => dateFormat.format(new Date(value))

  // «через 9 ч», «через 3 дн», «просрочено 4 ч назад»
  function formatTimeLeft(expiresAt) {
    const minutes = Math.round((new Date(expiresAt).getTime() - API.now()) / 60000)
    const abs = Math.abs(minutes)
    const span = abs < 60 ? abs + ' мин' : abs < 2880 ? Math.round(abs / 60) + ' ч' : Math.round(abs / 1440) + ' дн'
    return minutes >= 0 ? 'через ' + span : 'просрочено ' + span + ' назад'
  }

  // «меньше часа», «5 ч», «3 дн»
  function formatHours(hours) {
    if (hours < 1) return 'меньше часа'
    return hours < 48 ? Math.round(hours) + ' ч' : Math.round(hours / 24) + ' дн'
  }

  // 1 продукт, 3 продукта, 5 продуктов
  function plural(count, one, few, many) {
    const tens = count % 100
    const units = count % 10
    const word = tens > 10 && tens < 20 ? many : units === 1 ? one : units >= 2 && units <= 4 ? few : many
    return count + ' ' + word
  }

  const portionsLabel = (count) => plural(count, 'порция', 'порции', 'порций')

  const normalize = (value) => String(value ?? '').trim().toLowerCase().replaceAll('ё', 'е').replace(/\s+/g, ' ')

  /* ---------- Мелкие элементы интерфейса ---------- */

  const badge = (label, color) => `<span class="label status" style="--status-color:${color}">${label}</span>`

  const chevron = (open) =>
    `<svg class="chev${open ? ' is-open' : ''}" viewBox="0 0 12 12" aria-hidden="true"><path d="M4 2l4 4-4 4" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>`

  const loading = () => '<div class="empty"><span class="spinner" aria-label="Загрузка"></span></div>'

  const emptyState = (title, text, action) =>
    `<div class="glass empty rise">${title ? `<h3>${title}</h3>` : ''}<p>${text}</p>${action || ''}</div>`

  function setMessage(node, text, isError) {
    if (!node) return
    node.textContent = text || ''
    node.className = isError ? 'form-msg is-error' : 'form-msg'
  }

  // Удаление в два нажатия: первое взводит кнопку, второе подтверждает.
  function confirmClick(button) {
    if (button.dataset.armed) {
      clearTimeout(Number(button.dataset.armed))
      disarm(button)
      return true
    }
    button.dataset.text = button.textContent
    button.textContent = button.dataset.confirm || 'Точно удалить?'
    button.classList.replace('btn-line', 'btn-danger')
    button.dataset.armed = String(setTimeout(() => disarm(button), 4000))
    return false
  }

  function disarm(button) {
    if (!button.dataset.armed) return
    delete button.dataset.armed
    button.textContent = button.dataset.text
    button.classList.replace('btn-danger', 'btn-line')
  }

  /* ---------- Анимации ---------- */

  const reducedMotion = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches

  // Число «набегает» от прежнего значения к новому.
  function countUp(node, value, suffix) {
    if (!node) return
    const from = Number(node.dataset.value || 0)
    node.dataset.value = String(value)
    const tail = suffix || ''
    if (reducedMotion() || from === value) {
      node.textContent = numberFormat.format(value) + tail
      return
    }
    const started = performance.now()
    const step = (time) => {
      const progress = Math.min(1, (time - started) / 700)
      const eased = 1 - Math.pow(1 - progress, 3)
      node.textContent = numberFormat.format(Math.round(from + (value - from) * eased)) + tail
      if (progress < 1) requestAnimationFrame(step)
    }
    requestAnimationFrame(step)
  }

  // Порядковый номер для ступенчатого появления: style="--i:3"
  const order = (index) => `style="--i:${Math.min(index, 14)}"`

  // Элемент «перелетает» от одной точки к другой, например блюдо — в корзину.
  function fly(fromNode, toNode) {
    if (reducedMotion() || !fromNode || !toNode) return
    const from = fromNode.getBoundingClientRect()
    const to = toNode.getBoundingClientRect()
    const dot = document.createElement('span')
    dot.className = 'fly-dot'
    document.body.append(dot)
    const dx = to.left + to.width / 2 - (from.left + from.width / 2)
    const dy = to.top + 24 - (from.top + from.height / 2)
    dot.style.left = from.left + from.width / 2 + 'px'
    dot.style.top = from.top + from.height / 2 + 'px'
    dot
      .animate(
        [
          { transform: 'translate(-50%, -50%) scale(1)', opacity: 1 },
          { transform: `translate(calc(-50% + ${dx * 0.5}px), calc(-50% + ${dy * 0.5 - 70}px)) scale(1.2)`, opacity: 1, offset: 0.5 },
          { transform: `translate(calc(-50% + ${dx}px), calc(-50% + ${dy}px)) scale(0.3)`, opacity: 0.2 },
        ],
        { duration: 620, easing: 'cubic-bezier(0.3, 0, 0.2, 1)' },
      )
      .finished.then(() => dot.remove(), () => dot.remove())
  }

  // Короткая подсветка элемента, чьи данные только что изменились.
  function flash(node) {
    if (!node) return
    node.classList.remove('flash')
    void node.offsetWidth
    node.classList.add('flash')
  }

  /* ---------- Шапка, вход и роль ---------- */

  const NAV = [
    ['stock', 'Склад', 'sclad.html'],
    ['supplies', 'Поставки', 'supplies.html'],
    ['requests', 'Заявка', 'requests.html'],
    ['menu', 'Меню', 'app.html#/menu'],
    ['orders', 'Заказы', 'orders.html'],
    ['stoplist', 'Стоп-лист', 'stoplist.html'],
    ['hot-offers', 'Горячее', 'app.html#/hot-offers'],
    ['expiry', 'Сроки', 'app.html#/expiry'],
    ['analytics', 'Аналитика', 'analytics.html'],
    ['demo', 'Демо', 'app.html#/demo'],
  ]

  const isAdmin = () => {
    const user = API.getUser()
    return Boolean(user && user.role === 'admin')
  }

  // Шапка отвечает только за отображение и навигацию. Доступ к данным
  // проверяет API: страница не перенаправляет пользователя при 401/403.
  function shell(active) {
    const user = API.getUser()
    const admin = Boolean(user && user.role === 'admin')
    const app = document.getElementById('app')
    // Менеджер видит все данные, но кнопки изменения у него скрыты (см. [data-admin] в styles.css).
    app.classList.toggle('is-viewer', Boolean(user) && !admin)

    const links = NAV.map(([key, label, href]) => `<a href="${href}"${key === active ? ' class="active"' : ''}>${label}</a>`).join('')
    const old = app.querySelector('.nav')
    if (old) {
      old.nextElementSibling.remove()
      old.remove()
    }
    app.insertAdjacentHTML(
      'afterbegin',
      `<header class="nav">
        <a class="logo" href="index.html">Stoplist</a>
        <nav class="nav-links" aria-label="Основная навигация">${links}</nav>
        <div class="nav-actions">
          <span class="nav-user" title="${escapeHtml(user?.email || '')}">${escapeHtml(user?.name || 'Сессия определяется сервером')}</span>
          ${admin ? '<a class="btn btn-line btn-sm" href="app.html#/admin">Администрирование</a>' : ''}
          <span class="label tag clock-chip" id="clockChip" hidden></span>
          ${user ? '<button type="button" class="btn btn-line btn-sm" data-logout>Выйти</button>' : '<a class="btn btn-line btn-sm" href="login.html">Войти</a>'}
        </div>
        <button class="burger" id="burger" aria-label="Открыть меню" aria-expanded="false" aria-controls="menu"><span></span></button>
      </header>
      <nav class="menu" id="menu" aria-label="Мобильная навигация">
        ${links}<div class="divider"></div>
        ${admin ? '<a href="app.html#/admin">Администрирование</a>' : ''}
        ${user ? '<a href="index.html" data-logout>Завершить сессию</a>' : '<a href="login.html">Войти</a>'}
      </nav>`,
    )
    refreshRole(() => shell(active))
    return true
  }

  // Роль могли поменять, пока пользователь сидит на сайте: сверяем её с сервером
  // и при изменении перерисовываем шапку и доступные кнопки.
  let roleChecked = false
  function refreshRole(redraw) {
    if (roleChecked || !API.getUser()) return
    roleChecked = true
    API.get('/auth/me')
      .then((me) => {
        if (!me || me.role === API.getUser().role) return
        API.updateUser(me)
        redraw()
      })
      .catch(() => {})
  }

  function setMenu(open) {
    const menu = document.getElementById('menu')
    const burger = document.getElementById('burger')
    if (!menu || !burger) return
    menu.classList.toggle('open', open)
    burger.setAttribute('aria-expanded', String(open))
    burger.setAttribute('aria-label', open ? 'Закрыть меню' : 'Открыть меню')
  }

  document.addEventListener('click', (event) => {
    const menu = document.getElementById('menu')
    if (event.target.closest('[data-logout]')) {
      event.preventDefault()
      API.post('/auth/logout').catch(() => {})
      API.clearSession()
      location.href = 'index.html'
      return
    }
    if (event.target.closest('#burger')) {
      event.stopPropagation()
      setMenu(!menu.classList.contains('open'))
      return
    }
    if (menu && menu.classList.contains('open') && !event.target.closest('#menu')) setMenu(false)
  })

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') setMenu(false)
  })

  /* ---------- Запросы и автообновление ---------- */

  let seenRev = API.rev()

  // Ошибки API показываются вызывающей страницей; 401/403 не меняют маршрут.
  async function call(promise) {
    const result = await promise
    seenRev = API.rev()
    return result
  }

  const ALERT_COLOR = { warning: STATUS.warning.color, critical: STATUS.critical.color, expired: STATUS.expired.color, stop: STATUS.expired.color, ok: STATUS.ok.color }
  const shownAlerts = new Set()
  let toastsReady = false

  function showToasts(alerts) {
    let box = document.getElementById('toasts')
    if (!box) {
      box = document.createElement('div')
      box.id = 'toasts'
      box.className = 'toasts'
      box.setAttribute('aria-live', 'polite')
      document.body.append(box)
    }
    // При первом опросе только запоминаем уже существующие уведомления, не показывая их.
    const fresh = alerts.filter((alert) => !shownAlerts.has(alert.id))
    alerts.forEach((alert) => shownAlerts.add(alert.id))
    if (!toastsReady) {
      toastsReady = true
      return
    }
    for (const alert of fresh.reverse()) {
      const toast = document.createElement('div')
      toast.className = 'toast'
      toast.innerHTML = `<span class="status" style="--status-color:${ALERT_COLOR[alert.level] || '#fff'}"></span><span>${escapeHtml(alert.text)}</span>`
      box.append(toast)
      setTimeout(() => toast.remove(), 7000)
      while (box.children.length > 4) box.firstElementChild.remove()
    }
  }

  // Раз в три секунды сверяемся с сервером: если данные изменились, страница тихо обновляется.
  // С настоящим бэкендом вместо опроса сюда подключается поток событий (SSE).
  function live(onChange) {
    const tick = async () => {
      try {
        const state = await API.get('/pulse')
        const chip = document.getElementById('clockChip')
        if (chip) {
          chip.hidden = state.speed === 1
          chip.textContent = '×' + state.speed + ' · ' + formatDateTime(state.now)
        }
        showToasts(state.alerts || [])
        if (state.rev !== seenRev) {
          seenRev = state.rev
          onChange()
        }
      } catch {
        // При сетевой ошибке ничего не перенаправляем; следующий опрос повторит запрос.
      }
    }
    tick()
    setInterval(tick, 3000)
  }

  return {
    escapeHtml, formatQuantity, formatDateTime, formatTimeLeft, formatHours, plural, portionsLabel, normalize,
    UNIT_LABEL, STATUS, badge, chevron, loading, emptyState, setMessage, confirmClick, disarm,
    countUp, order, fly, flash, reducedMotion,
    shell, isAdmin, call, live,
  }
})()
