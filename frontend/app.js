/* Экраны и маршруты приложения. Данные приходят только через API (см. api.js). */
;(() => {
  const view = document.getElementById('view')
  const app = document.getElementById('app')

  /* ---------- Вспомогательное ---------- */

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

  function remainingShare(receivedAt, expiresAt) {
    const start = new Date(receivedAt).getTime()
    const end = new Date(expiresAt).getTime()
    if (end <= start) return 0
    return Math.min(1, Math.max(0, (end - API.now()) / (end - start)))
  }

  const statusBadge = (status) =>
    `<span class="label status" style="--status-color:${STATUS[status].color}">${STATUS[status].label}</span>`

  const chevron = (open) =>
    `<svg class="chev${open ? ' is-open' : ''}" viewBox="0 0 12 12" aria-hidden="true"><path d="M4 2l4 4-4 4" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg>`

  const pageHeader = (title, description) =>
    `<div class="page-head"><h1 class="page-title">${escapeHtml(title)}</h1>${description ? `<p class="page-sub">${escapeHtml(description)}</p>` : ''}<p class="viewer-note label">Режим просмотра — изменять данные могут владелец и директор ресторана</p></div>`

  const loading = () => '<div class="empty"><span class="spinner" aria-label="Загрузка"></span></div>'

  function setMessage(node, text, isError) {
    if (!node) return
    node.textContent = text || ''
    node.className = isError ? 'form-msg is-error' : 'form-msg'
  }

  // Удаление в два нажатия: первое взводит кнопку, второе подтверждает.
  // Возвращает true, когда действие подтверждено.
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

  // Ответы 401/403 обрабатываются экраном; API не меняет маршрут пользователя.
  async function call(promise) {
    const result = await promise
    seenRev = API.rev()
    return result
  }

  /* ---------- Навигация ---------- */

  // Разделы с третьим элементом живут в отдельных файлах, остальные — экраны этой страницы.
  const NAV = [
    ['stock', 'Склад', 'sclad.html'],
    ['supplies', 'Поставки', 'supplies.html'],
    ['requests', 'Заявка', 'requests.html'],
    ['menu', 'Меню'],
    ['orders', 'Заказы', 'orders.html'],
    ['stoplist', 'Стоп-лист', 'stoplist.html'],
    ['hot-offers', 'Горячее'],
    ['expiry', 'Сроки'],
    ['analytics', 'Аналитика', 'analytics.html'],
    ['demo', 'Демо'],
  ]
  const PAGES = Object.fromEntries(NAV.filter((item) => item[2]).map((item) => [item[0], item[2]]))

  // Владелец и директор — полные права в ресторане.
  const isAdmin = () => {
    const user = API.getUser()
    return Boolean(user && (user.role === 'owner' || user.role === 'director'))
  }
  const isOwner = () => (API.getUser() || {}).role === 'owner'

  // Текущий ресторан в шапке: владелец может его сменить, сотрудники видят свой.
  function restaurantChip(user) {
    const restaurant = API.getRestaurant()
    if (!user || !restaurant || user.role === 'supplier') return ''
    return user.role === 'owner'
      ? `<a class="btn btn-line btn-sm" href="#/restaurants" title="Сменить ресторан">${escapeHtml(restaurant.name)} · сменить</a>`
      : `<span class="label tag">${escapeHtml(restaurant.name)}</span>`
  }

  const burger = document.getElementById('burger')
  const menu = document.getElementById('menu')

  function renderNav(current) {
    const user = API.getUser()
    const waiterTabs = user && user.role === 'waiter' ? ['orders', 'hot-offers'] : null
    const links = NAV.filter(([path]) => !waiterTabs || waiterTabs.includes(path)).map(([path, label, page]) => `<a href="${page || '#/' + path}"${path === current ? ' class="active"' : ''}>${label}</a>`).join('') +
      (user ? '' : `<a href="#/register"${current === 'register' ? ' class="active"' : ''}>Зарегистрироваться</a>`)
    document.getElementById('navLinks').innerHTML = links
    const admin = isAdmin()
    document.getElementById('navActions').innerHTML =
      `<span class="nav-user" title="${escapeHtml(user?.email || '')}">${escapeHtml(user?.name || 'Сессия определяется сервером')}</span>
       ${restaurantChip(user)}
       ${admin ? `<a class="btn btn-line btn-sm${current === 'admin' ? ' active' : ''}" href="#/admin">Администрирование</a>` : ''}
       <span class="label tag clock-chip" id="clockChip" hidden></span>
       ${user ? '<button type="button" class="btn btn-line btn-sm" data-logout>Выйти</button>' : '<a class="btn btn-solid btn-sm" href="login.html">Войти</a>'}`
    menu.innerHTML = links + '<div class="divider"></div>' +
      (isOwner() ? '<a href="#/restaurants">Сменить ресторан</a>' : '') +
      (admin ? '<a href="#/admin">Администрирование</a>' : '') +
      (user ? '<a href="index.html" data-logout>Завершить сессию</a>' : '<a href="login.html">Войти</a>')
  }

  function setMenu(open) {
    menu.classList.toggle('open', open)
    burger.setAttribute('aria-expanded', String(open))
    burger.setAttribute('aria-label', open ? 'Закрыть меню' : 'Открыть меню')
  }

  burger.addEventListener('click', (event) => {
    event.stopPropagation()
    setMenu(!menu.classList.contains('open'))
  })

  document.addEventListener('click', (event) => {
    const logout = event.target.closest('[data-logout]')
    if (logout) {
      event.preventDefault()
      API.post('/auth/logout').catch(() => {})
      API.clearSession()
      setMenu(false)
      location.href = 'index.html'
      return
    }
    if (menu.classList.contains('open') && (event.target.closest('#menu a') || !event.target.closest('#menu'))) setMenu(false)
  })

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && menu.classList.contains('open')) {
      setMenu(false)
      burger.focus()
    }
  })

  /* ---------- Главная ---------- */

  // 1 продукт, 3 продукта, 5 продуктов
  function plural(count, one, few, many) {
    const tens = count % 100
    const units = count % 10
    const word = tens > 10 && tens < 20 ? many : units === 1 ? one : units >= 2 && units <= 4 ? few : many
    return count + ' ' + word
  }

  /* ---------- Вход и регистрация ---------- */

  function authView(mode, query) {
    const isRegister = mode === 'register'
    const next = /^[a-z-]+$/.test(query.get('next') || '') ? query.get('next') : 'stock'
    view.innerHTML = `
      <section class="auth">
        <div class="auth-main">
          <div class="auth-card">
            <span class="label home-badge">${isRegister ? 'Новый аккаунт' : 'С возвращением'}</span>
            <h1>${isRegister ? 'Регистрация' : 'Вход'}</h1>
            <p class="page-sub">${isRegister ? 'Создайте аккаунт, чтобы вести склад и принимать поставки.' : 'Войдите, чтобы открыть склад и поставки.'}</p>
            <form id="authForm" novalidate>
              ${
                isRegister
                  ? '<label class="field"><span class="label">Имя</span><input class="input" name="name" autocomplete="name" placeholder="Как к вам обращаться" /></label>'
                  : ''
              }
              <label class="field"><span class="label">${isRegister ? 'Почта' : 'Почта или логин'}</span><input class="input" name="email" type="${isRegister ? 'email' : 'text'}" autocomplete="${isRegister ? 'email' : 'username'}" placeholder="${isRegister ? 'you@example.com' : 'you@example.com или логин'}" /></label>
              <label class="field"><span class="label">Пароль</span><input class="input" name="password" type="password" autocomplete="${isRegister ? 'new-password' : 'current-password'}" placeholder="${isRegister ? 'Не короче 6 символов' : 'Ваш пароль'}" /></label>
              ${
                isRegister
                  ? '<label class="field"><span class="label">Пароль ещё раз</span><input class="input" name="repeat" type="password" autocomplete="new-password" placeholder="Повторите пароль" /></label>'
                  : ''
              }
              <button type="submit" class="btn btn-solid">${isRegister ? 'Создать аккаунт' : 'Войти'}</button>
              <span class="form-msg" id="authMsg" role="status"></span>
            </form>
            <p class="auth-switch">
              ${
                isRegister
                  ? `Уже есть аккаунт? <a href="login.html?next=${encodeURIComponent(next)}">Войти</a>`
                  : `<span id="registerHint">Нет аккаунта? Его выдаёт владелец или директор ресторана.</span>`
              }
            </p>
          </div>
        </div>
        <aside class="auth-art" aria-hidden="true">
          <div class="panel-bg"><i></i><i></i><span class="grain"></span></div>
          <div class="panel-caption">
            <h3>Каждая партия, каждое блюдо — один ясный сигнал</h3>
            <p>Остатки, сроки годности и стоп-лист в одном месте</p>
          </div>
        </aside>
      </section>`

    const form = document.getElementById('authForm')
    const message = document.getElementById('authMsg')
    form.querySelector('input').focus()

    // Регистрация открыта только для первых двух администраторов.
    API.get('/auth/registration')
      .then((state) => {
        const hint = document.getElementById('registerHint')
        if (hint && state.open) hint.innerHTML = `Нет аккаунта? <a href="#/register?next=${encodeURIComponent(next)}">Зарегистрироваться</a>`
        if (isRegister && !state.open && document.body.contains(form)) {
          form.outerHTML = '<div class="glass empty" style="margin-top:24px"><h3>Регистрация закрыта</h3><p>Владельцы уже зарегистрированы. Аккаунты сотрудников выдают владелец и директор ресторана на странице «Администрирование».</p></div>'
        }
      })
      .catch(() => {})

    // Проверяем доступность API. Состояние сессии и права определяет backend.
    API.get('/health').catch((error) => {
      if (!document.body.contains(form)) return
      form.insertAdjacentHTML('afterend', `
        <div class="glass empty" style="margin-top:16px">
          <h3>Сервер недоступен</h3>
          <p>${escapeHtml(error.message)}</p>
        </div>`)
    })

    form.addEventListener('submit', async (event) => {
      event.preventDefault()
      const data = Object.fromEntries(new FormData(form))
      form.querySelectorAll('.input').forEach((input) => input.classList.remove('is-invalid'))

      // Проверка на клиенте — только для удобства, сервер проверяет всё заново.
      const invalid = []
      if (isRegister && !data.name.trim()) invalid.push(['name', 'Укажите имя'])
      if (isRegister ? !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(data.email.trim()) : !data.email.trim()) invalid.push(['email', isRegister ? 'Укажите корректную почту' : 'Введите почту или логин'])
      if (isRegister ? data.password.length < 6 : !data.password) invalid.push(['password', isRegister ? 'Пароль должен быть не короче 6 символов' : 'Введите пароль'])
      if (isRegister && data.password !== data.repeat) invalid.push(['repeat', 'Пароли не совпадают'])
      if (invalid.length) {
        invalid.forEach(([name]) => form.elements[name].classList.add('is-invalid'))
        form.elements[invalid[0][0]].focus()
        setMessage(message, invalid[0][1], true)
        return
      }

      const submit = form.querySelector('[type=submit]')
      submit.disabled = true
      setMessage(message, '')
      try {
        const body = { email: data.email.trim(), password: data.password }
        if (isRegister) body.name = data.name.trim()
        API.setSession(await API.post(isRegister ? '/auth/register' : '/auth/login', body))
        // Часть разделов — отдельные страницы: маршрутизатор сам отправит куда нужно.
        location.hash = '#/' + next
      } catch (error) {
        setMessage(message, error.message, true)
        submit.disabled = false
      }
    })
  }

  /* ---------- Меню: блюда и их состав ---------- */

  const portionsLabel = (count) => plural(count, 'порция', 'порции', 'порций')

  // «Моцарелла: нужно 150 г, есть 80 г»
  const limitText = (limit) =>
    `${escapeHtml(limit.productName)}: нужно ${formatQuantity(limit.required, limit.unit)}, есть ${formatQuantity(limit.available, limit.unit)}`

  // Для блюд, которые ещё можно приготовить: «Моцарелла: уходит 150 г на порцию, осталось 200 г»
  const runningText = (limit) =>
    `${escapeHtml(limit.productName)}: уходит ${formatQuantity(limit.required, limit.unit)} на порцию, осталось ${formatQuantity(limit.available, limit.unit)}`

  const moneyFormat = new Intl.NumberFormat('ru-RU', { style: 'currency', currency: 'RUB', maximumFractionDigits: 2 })

  // Цена порции: администратор задаёт её прямо в таблице, менеджер только видит.
  function dishPriceCell(dish) {
    const value = dish.priceConfigured ? (dish.priceKopecks / 100).toFixed(2).replace(/\.00$/, '') : ''
    if (!isAdmin()) return dish.priceConfigured ? moneyFormat.format(dish.priceKopecks / 100) : '<span class="dim">не задана</span>'
    return `<div class="field with-unit" style="width:150px"><input class="input" data-dish-price type="number" min="0" step="0.01" inputmode="decimal" value="${value}" placeholder="0" aria-label="Цена порции, ₽" /><span>₽</span></div>`
  }

  const stopBadge = (dish) =>
    dish.stopped
      ? `<span class="label status" style="--status-color:${STATUS.expired.color}">В стопе</span>`
      : `<span class="label status" style="--status-color:${STATUS.ok.color}">Доступно</span>`

  function ingredientRowHtml(products) {
    return `<div class="supply-row is-recipe">
      <div class="field"><select class="input" name="productId" aria-label="Продукт">
        <option value="">Выберите продукт</option>
        ${products.map((product) => `<option value="${escapeHtml(product.id)}" data-unit="${product.unit}">${escapeHtml(product.name)}</option>`).join('')}
      </select></div>
      <div class="field with-unit"><input class="input" name="quantity" type="number" min="1" inputmode="numeric" aria-label="Количество на порцию" placeholder="0" /><span data-unit-label></span></div>
      <button type="button" class="icon-btn" data-remove-row aria-label="Убрать продукт из состава" title="Убрать продукт">×</button>
    </div>`
  }

  async function menuView() {
    view.innerHTML = `<main class="page">
      ${pageHeader('Меню', 'Блюда и их состав. По составу считается, сколько порций можно приготовить и что попадает в стоп-лист.')}
      <div id="dishFormWrap" data-admin>${loading()}</div>
      <div class="section-gap">
        <div class="form-head" style="margin-bottom:16px">
          <h2 class="panel-title">Блюда</h2>
          <span class="form-msg" id="dishListMsg" role="status"></span>
        </div>
        <div id="dishList">${loading()}</div>
      </div>
    </main>`

    const formWrap = document.getElementById('dishFormWrap')
    const list = document.getElementById('dishList')
    const listMsg = document.getElementById('dishListMsg')
    let products = []

    async function loadDishes() {
      let menu
      try {
        menu = await call(API.get('/menu'))
      } catch (error) {
        list.innerHTML = `<div class="glass empty"><h3>Не удалось загрузить меню</h3><p>${escapeHtml(error.message)}</p></div>`
        return
      }
      if (menu.length === 0) {
        list.innerHTML = '<div class="glass empty"><h3>В меню пока нет блюд</h3><p>Добавьте первое блюдо в форме выше.</p></div>'
        return
      }
      list.innerHTML = `<div class="glass table-wrap"><table class="table">
        <thead><tr><th>Блюдо</th><th>Состав на порцию</th><th class="num">Можно приготовить</th><th>Цена порции</th><th>Статус</th><th></th></tr></thead>
        <tbody>
          ${menu
            .map(
              (dish) => `<tr data-dish="${escapeHtml(dish.dishId)}" data-name="${escapeHtml(dish.name)}">
                <td class="strong">${escapeHtml(dish.name)}</td>
                <td class="dim">${dish.ingredients.map((item) => `${escapeHtml(item.name)} — ${formatQuantity(item.quantity, item.unit)}`).join(' · ')}</td>
                <td class="num nowrap">${portionsLabel(dish.availablePortions)}</td>
                <td class="nowrap">${dishPriceCell(dish)}</td>
                <td>${stopBadge(dish)}${dish.stopReason ? `<div class="cell-note">${limitText(dish.stopReason)}</div>` : ''}</td>
                <td class="num"><button type="button" class="btn btn-sm btn-line" data-delete-dish data-admin>Удалить</button></td>
              </tr>`,
            )
            .join('')}
        </tbody>
      </table></div>`
    }

    function renderForm() {
      if (products.length === 0) {
        formWrap.innerHTML = `<div class="glass empty">
          <h3>Сначала нужны продукты</h3>
          <p>Состав блюда выбирается из продуктов склада. Примите поставку — и они появятся в списке.</p>
          <a class="btn btn-solid" href="supplies.html" data-admin>Принять поставку</a>
        </div>`
        return
      }
      formWrap.innerHTML = `<form class="glass form" id="dishForm" novalidate>
        <h2 class="panel-title">Новое блюдо</h2>
        <label class="field" style="max-width:420px">
          <span class="label">Название</span>
          <input class="input" id="dishName" placeholder="Например, пицца «Маргарита»" autocomplete="off" />
        </label>
        <label class="field" style="max-width:200px">
          <span class="label">Цена порции</span>
          <span class="field with-unit"><input class="input" id="dishPrice" type="number" min="0" step="0.01" inputmode="decimal" placeholder="0" /><span>₽</span></span>
        </label>
        <div class="supply-rows">
          <div class="supply-row is-recipe is-head"><span class="label">Продукт</span><span class="label">На одну порцию</span><span></span></div>
          <div id="dishRows">${ingredientRowHtml(products)}</div>
        </div>
        <div class="form-foot">
          <button type="submit" class="btn btn-solid">Добавить блюдо</button>
          <button type="button" class="btn btn-line" id="addIngredientBtn">Добавить продукт</button>
          <span class="form-msg" id="dishMsg" role="status"></span>
        </div>
      </form>`

      const form = document.getElementById('dishForm')
      const rows = document.getElementById('dishRows')
      const nameInput = document.getElementById('dishName')
      const message = document.getElementById('dishMsg')
      const priceInput = document.getElementById('dishPrice')

      rows.addEventListener('input', (event) => event.target.classList.remove('is-invalid'))
      rows.addEventListener('change', (event) => {
        if (event.target.name !== 'productId') return
        const option = event.target.selectedOptions[0]
        event.target.closest('.supply-row').querySelector('[data-unit-label]').textContent = UNIT_LABEL[option.dataset.unit] || ''
      })
      form.addEventListener('click', (event) => {
        const remove = event.target.closest('[data-remove-row]')
        if (!remove) return
        if (rows.children.length === 1) rows.innerHTML = ingredientRowHtml(products)
        else remove.closest('.supply-row').remove()
      })
      document.getElementById('addIngredientBtn').addEventListener('click', () => {
        rows.insertAdjacentHTML('beforeend', ingredientRowHtml(products))
        rows.lastElementChild.querySelector('select').focus()
      })

      form.addEventListener('submit', async (event) => {
        event.preventDefault()
        let valid = true
        const mark = (input, bad) => {
          input.classList.toggle('is-invalid', bad)
          if (bad) valid = false
        }
        mark(nameInput, !nameInput.value.trim())
        mark(priceInput, priceInput.value !== '' && !(Number(priceInput.value) >= 0))
        const ingredients = [...rows.querySelectorAll('.supply-row')].map((row) => {
          const product = row.querySelector('[name=productId]')
          const quantity = row.querySelector('[name=quantity]')
          mark(product, !product.value)
          mark(quantity, !(Number(quantity.value) > 0))
          return { productId: product.value, quantity: Number(quantity.value) }
        })
        if (!valid) {
          setMessage(message, 'Заполните название, продукт и количество в каждой строке', true)
          return
        }

        const submit = form.querySelector('[type=submit]')
        submit.disabled = true
        try {
          const dish = await call(API.post('/dishes', { name: nameInput.value.trim(), ingredients }))
          if (priceInput.value !== '') await call(API.post('/dishes/' + encodeURIComponent(dish.dishId) + '/price', { priceKopecks: Math.round(Number(priceInput.value) * 100) }))
          setMessage(message, `Блюдо добавлено: ${dish.name}. Можно приготовить: ${portionsLabel(dish.availablePortions)}`)
          nameInput.value = ''
          priceInput.value = ''
          rows.innerHTML = ingredientRowHtml(products)
          await loadDishes()
        } catch (error) {
          setMessage(message, error.message, true)
        } finally {
          submit.disabled = false
        }
      })
    }

    list.addEventListener('change', async (event) => {
      const input = event.target.closest('[data-dish-price]')
      if (!input) return
      const row = input.closest('tr')
      const price = Number(input.value)
      if (input.value === '' || !(price >= 0)) {
        input.classList.add('is-invalid')
        setMessage(listMsg, 'Укажите цену порции в рублях', true)
        return
      }
      input.classList.remove('is-invalid')
      input.disabled = true
      try {
        await call(API.post('/dishes/' + encodeURIComponent(row.dataset.dish) + '/price', { priceKopecks: Math.round(price * 100) }))
        setMessage(listMsg, `Цена «${row.dataset.name}»: ${moneyFormat.format(Math.round(price * 100) / 100)} за порцию`)
      } catch (error) {
        setMessage(listMsg, error.message, true)
      } finally {
        input.disabled = false
      }
    })

    list.addEventListener('click', async (event) => {
      const button = event.target.closest('[data-delete-dish]')
      if (!button || !confirmClick(button)) return
      const row = button.closest('tr')
      button.disabled = true
      try {
        await call(API.del('/dishes/' + encodeURIComponent(row.dataset.dish)))
        setMessage(listMsg, `Блюдо удалено: ${row.dataset.name}`)
        await loadDishes()
      } catch (error) {
        setMessage(listMsg, error.message, true)
        button.disabled = false
      }
    })

    products = await call(API.get('/products')).catch(() => [])
    if (!document.body.contains(formWrap)) return
    renderForm()
    refreshView = loadDishes
    await loadDishes()
  }

  /* ---------- Администрирование: кто может изменять данные ---------- */

  const ROLE_LABEL = { owner: 'Владелец', director: 'Директор', manager: 'Менеджер', waiter: 'Официант', supplier: 'Поставщик' }
  const roleBadge = (role) =>
    `<span class="label status" style="--status-color:${role === 'owner' || role === 'director' ? STATUS.ok.color : 'rgba(255,255,255,0.3)'}">${ROLE_LABEL[role] || role}</span>`

  /* ---------- Выбор ресторана (владелец) ---------- */

  async function restaurantsView() {
    view.innerHTML = `<main class="page">
      ${pageHeader('Рестораны', 'Выберите ресторан — в нём доступны все разделы сайта с полными правами. Сменить ресторан можно в шапке.')}
      <div id="restaurantList">${loading()}</div>
    </main>`
    const list = document.getElementById('restaurantList')
    let restaurants
    try {
      restaurants = await call(API.get('/restaurants'))
    } catch (error) {
      list.innerHTML = `<div class="glass empty"><h3>Не удалось загрузить рестораны</h3><p>${escapeHtml(error.message)}</p></div>`
      return
    }
    const current = API.getRestaurant()
    list.innerHTML = `<div class="stop-summary">${restaurants
      .map((restaurant) => `<button type="button" class="glass stat" data-restaurant="${escapeHtml(restaurant.id)}" data-name="${escapeHtml(restaurant.name)}"
          style="cursor:pointer;text-align:left;font:inherit;color:inherit${current && current.id === restaurant.id ? ';outline:1px solid rgba(255,255,255,0.6)' : ''}">
          <div class="big" style="font-size:28px">${escapeHtml(restaurant.name)}</div>
          <div class="small">${current && current.id === restaurant.id ? 'сейчас открыт · войти' : 'войти'}</div>
        </button>`)
      .join('')}</div>`
    list.addEventListener('click', (event) => {
      const card = event.target.closest('[data-restaurant]')
      if (!card) return
      API.setRestaurant({ id: card.dataset.restaurant, name: card.dataset.name })
      location.href = 'index.html'
    })
  }

  /* ---------- Администрирование: сотрудники сети ---------- */

  async function adminView() {
    // Сотрудников заводят владелец (во всех ресторанах) и директор (в своём).
    if (!isAdmin()) {
      view.innerHTML = `<main class="page">${pageHeader('Администрирование', '')}<div class="glass empty"><h3>Нет прав</h3><p>Управлять сотрудниками могут владелец и директор ресторана.</p></div></main>`
      return
    }
    const me = API.getUser()
    const owner = me.role === 'owner'
    const roles = owner ? ['director', 'manager', 'waiter', 'supplier'] : ['manager', 'waiter']
    let restaurants = []
    if (owner) restaurants = await call(API.get('/restaurants')).catch(() => [])
    const current = API.getRestaurant()
    view.innerHTML = `<main class="page">
      ${pageHeader('Администрирование', owner
        ? 'Владелец заводит директоров (по одному на ресторан), менеджеров и официантов любого ресторана и общего поставщика. Директор полностью управляет своим рестораном и заводит его менеджеров и официантов.'
        : `Сотрудники ресторана «${escapeHtml(me.restaurantName || '')}». Менеджер видит данные и рассматривает поставки, официант оформляет заказы.`)}
      <form class="glass form" id="managerForm" novalidate style="margin-bottom:32px">
        <h2 class="panel-title">Добавить сотрудника</h2>
        <div class="supply-row" style="grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) minmax(0, 0.8fr)${owner ? ' minmax(0, 0.8fr)' : ''} auto">
          <label class="field"><span class="label">Логин</span><input class="input" name="login" autocomplete="off" placeholder="Например, ivanov" /></label>
          <label class="field"><span class="label">Пароль</span><input class="input" name="password" type="password" autocomplete="new-password" placeholder="Не короче 6 символов" /></label>
          <label class="field"><span class="label">Роль</span><select class="input" name="role">
            ${roles.map((role) => `<option value="${role}"${role === 'manager' ? ' selected' : ''}>${ROLE_LABEL[role]}</option>`).join('')}
          </select></label>
          ${owner ? `<label class="field" data-restaurant-field><span class="label">Ресторан</span><select class="input" name="restaurantId">
            ${restaurants.map((restaurant) => `<option value="${escapeHtml(restaurant.id)}"${current && current.id === restaurant.id ? ' selected' : ''}>${escapeHtml(restaurant.name)}</option>`).join('')}
          </select></label>` : ''}
          <button type="submit" class="btn btn-solid">Создать</button>
        </div>
        <span class="form-msg" id="managerMsg" role="status"></span>
      </form>
      <div class="form-head" style="margin-bottom:16px">
        <h2 class="panel-title">Пользователи</h2>
        <span class="form-msg" id="usersMsg" role="status"></span>
      </div>
      <div id="usersBody">${loading()}</div>
    </main>`

    const body = document.getElementById('usersBody')
    const message = document.getElementById('usersMsg')
    // Кого можно удалить: владелец — любого, кроме себя; директор — менеджеров и официантов.
    const canDelete = (user) => user.id !== me.id && (owner ? user.role !== 'owner' : user.role === 'manager' || user.role === 'waiter')

    async function loadUsers() {
      let users
      try {
        users = await call(API.get('/users'))
      } catch (error) {
        body.innerHTML = `<div class="glass empty"><h3>Не удалось загрузить пользователей</h3><p>${escapeHtml(error.message)}</p></div>`
        return
      }
      const count = (role) => users.filter((user) => user.role === role).length
      body.innerHTML = `
        <div class="stop-summary" style="margin-bottom:20px">
          <div class="glass stat"><div class="big">${users.length}</div><div class="small">всего аккаунтов</div></div>
          <div class="glass stat"><div class="big">${owner ? count('director') : count('manager')}</div><div class="small">${owner ? 'директоров' : 'менеджеров'}</div></div>
          <div class="glass stat"><div class="big">${count('waiter')}</div><div class="small">официантов</div></div>
        </div>
        <div class="glass table-wrap"><table class="table">
          <thead><tr><th>Имя</th><th>Логин</th><th>Роль</th><th>Ресторан</th><th></th></tr></thead>
          <tbody>
            ${users
              .map((user) => `<tr data-user="${escapeHtml(user.id)}" data-name="${escapeHtml(user.name)}">
                  <td class="strong">${escapeHtml(user.name)}</td>
                  <td class="dim">${escapeHtml(user.email)}</td>
                  <td>${roleBadge(user.role)}</td>
                  <td class="dim">${user.restaurantName ? escapeHtml(user.restaurantName) : 'все рестораны'}</td>
                  <td class="num">${user.id === me.id ? '<span class="dim">это вы</span>' : canDelete(user) ? '<button type="button" class="btn btn-sm btn-line" data-delete data-confirm="Точно удалить?">Удалить</button>' : ''}</td>
                </tr>`)
              .join('')}
          </tbody>
        </table></div>
        ${users.length === 1 ? '<p class="page-sub" style="margin-top:16px">Других аккаунтов пока нет. Добавьте сотрудников формой выше.</p>' : ''}`
    }

    const managerForm = document.getElementById('managerForm')
    const managerMsg = document.getElementById('managerMsg')
    const restaurantField = managerForm.querySelector('[data-restaurant-field]')
    // Поставщик общий для всех ресторанов — ресторан ему не выбирается.
    const syncRestaurantField = () => {
      if (restaurantField) restaurantField.style.visibility = managerForm.elements.role.value === 'supplier' ? 'hidden' : 'visible'
    }
    managerForm.elements.role.addEventListener('change', syncRestaurantField)
    syncRestaurantField()
    managerForm.addEventListener('submit', async (event) => {
      event.preventDefault()
      const login = managerForm.elements.login.value.trim().toLowerCase()
      const password = managerForm.elements.password.value
      const role = managerForm.elements.role.value
      const restaurantId = owner && role !== 'supplier' ? managerForm.elements.restaurantId.value : undefined
      managerForm.querySelectorAll('.input').forEach((input) => input.classList.remove('is-invalid'))
      if (!/^[a-z0-9._-]{3,64}$/.test(login)) {
        managerForm.elements.login.classList.add('is-invalid')
        setMessage(managerMsg, 'Логин: от 3 до 64 символов — латиница, цифры, точка, дефис или подчёркивание', true)
        return
      }
      if (password.length < 6) {
        managerForm.elements.password.classList.add('is-invalid')
        setMessage(managerMsg, 'Пароль должен быть не короче 6 символов', true)
        return
      }
      const submit = managerForm.querySelector('[type=submit]')
      submit.disabled = true
      try {
        const user = await call(API.post('/users', { login, password, role, restaurantId }))
        setMessage(managerMsg, `${ROLE_LABEL[role]} ${login}${user.restaurantName ? ' (' + user.restaurantName + ')' : ''} создан — можно войти с этим логином и паролем`)
        managerForm.reset()
        syncRestaurantField()
        await loadUsers()
      } catch (error) {
        setMessage(managerMsg, error.message, true)
      } finally {
        submit.disabled = false
      }
    })

    body.addEventListener('click', async (event) => {
      const remove = event.target.closest('[data-delete]')
      if (!remove || !confirmClick(remove)) return
      const row = remove.closest('tr')
      remove.disabled = true
      try {
        await call(API.del('/users/' + encodeURIComponent(row.dataset.user)))
        setMessage(message, `${row.dataset.name} удалён`)
        await loadUsers()
      } catch (error) {
        setMessage(message, error.message, true)
        remove.disabled = false
      }
    })

    await loadUsers()
  }

  /* ---------- Сроки годности и уведомления ---------- */

  const ALERT_COLOR = { warning: STATUS.warning.color, critical: STATUS.critical.color, expired: STATUS.expired.color, stop: STATUS.expired.color, ok: STATUS.ok.color }

  // «меньше часа», «5 ч», «3 дн»
  function formatHours(hours) {
    if (hours < 1) return 'меньше часа'
    return hours < 48 ? Math.round(hours) + ' ч' : Math.round(hours / 24) + ' дн'
  }

  const alertItem = (alert) =>
    `<li><span class="status" style="--status-color:${ALERT_COLOR[alert.level] || '#fff'}"></span><span>${escapeHtml(alert.text)}</span><time class="dim">${formatDateTime(alert.at)}</time></li>`

  let expiryFilter = 'all'

  async function expiryView(silent) {
    if (!silent) {
      view.innerHTML = `<main class="page">${pageHeader('Сроки годности', 'Все партии по срочности. Просроченное списывается автоматически, о приближении срока приходят уведомления.')}<div id="expiryBody">${loading()}</div></main>`
    }
    const body = document.getElementById('expiryBody')
    if (!body) return

    let batches, alerts
    try {
      ;[batches, alerts] = await Promise.all([call(API.get('/expiry')), call(API.get('/alerts'))])
    } catch (error) {
      body.innerHTML = `<div class="glass empty"><h3>Не удалось загрузить сроки</h3><p>${escapeHtml(error.message)}</p></div>`
      return
    }
    if (!document.body.contains(body)) return

    if (batches.length === 0 && alerts.length === 0) {
      body.innerHTML = `<div class="glass empty"><h3>Партий пока нет</h3><p>Сроки появятся здесь после приёмки первой поставки.</p><a class="btn btn-solid" href="supplies.html" data-admin>Принять поставку</a></div>`
      return
    }

    const active = batches.filter((batch) => batch.qtyRemaining > 0)
    const count = (status) => active.filter((batch) => batch.status === status).length
    const written = batches.filter((batch) => batch.writtenOff > 0)
    const filters = [['all', 'Все', active.length], ['critical', 'Истекает', count('critical')], ['warning', 'Скоро истекает', count('warning')], ['ok', 'В норме', count('ok')], ['written', 'Списано', written.length]]
    const shown = expiryFilter === 'all' ? active : expiryFilter === 'written' ? written : active.filter((batch) => batch.status === expiryFilter)

    body.innerHTML = `
      <div class="stop-summary">
        <div class="glass stat"><div class="big">${count('critical')}</div><div class="small">партий истекает</div></div>
        <div class="glass stat"><div class="big">${count('warning')}</div><div class="small">скоро истекает</div></div>
        <div class="glass stat"><div class="big">${written.length}</div><div class="small">списано по сроку</div></div>
      </div>

      <div class="chips" role="tablist">
        ${filters.map(([key, label, total]) => `<button type="button" class="chip${key === expiryFilter ? ' is-on' : ''}" data-filter="${key}">${label} <span>${total}</span></button>`).join('')}
      </div>
      ${
        shown.length
          ? `<div class="glass table-wrap"><table class="table">
              <thead><tr><th>Продукт</th><th>Поставка</th><th class="num">${expiryFilter === 'written' ? 'Списано' : 'Остаток'}</th><th>Срок хранения</th><th>Статус</th></tr></thead>
              <tbody>
                ${shown
                  .map(
                    (batch) => `<tr>
                      <td class="strong">${escapeHtml(batch.name)}</td>
                      <td class="dim">${escapeHtml(batch.supplyNumber)}</td>
                      <td class="num nowrap">${formatQuantity(expiryFilter === 'written' ? batch.writtenOff : batch.qtyRemaining, batch.unit)}</td>
                      <td><div class="shelf" style="--status-color:${STATUS[batch.status].color}">
                        <div class="shelf-track"><div class="shelf-range" style="width:${Math.round(remainingShare(batch.receivedAt, batch.expiresAt) * 100)}%"></div></div>
                        <span class="shelf-text">${formatTimeLeft(batch.expiresAt)}</span>
                      </div></td>
                      <td>${batch.qtyRemaining > 0 ? statusBadge(batch.status) : `<span class="label status" style="--status-color:${STATUS.expired.color}">Списано</span>`}</td>
                    </tr>`,
                  )
                  .join('')}
              </tbody>
            </table></div>`
          : '<div class="glass empty"><p>В этой группе партий нет.</p></div>'
      }

      <h2 class="panel-title" style="margin:36px 0 16px">Уведомления</h2>
      ${alerts.length ? `<ul class="glass feed">${alerts.map(alertItem).join('')}</ul>` : '<div class="glass empty"><p>Уведомлений пока нет.</p></div>'}`

    body.onclick = (event) => {
      const chip = event.target.closest('[data-filter]')
      if (!chip) return
      expiryFilter = chip.dataset.filter
      expiryView(true)
    }
  }

  /* ---------- Горячие предложения ---------- */

  // Объяснение собирается из тех же чисел, по которым блюдо попало в список.
  function reasonText(reason) {
    const parts = [
      `${escapeHtml(reason.productName)} из поставки ${escapeHtml(reason.supplyNumber)} истекает ${formatTimeLeft(reason.expiresAt)}.`,
      `На складе ${formatQuantity(reason.batchQuantity, reason.unit)}, при текущих продажах останется ${formatQuantity(reason.atRisk, reason.unit)}.`,
      `На порцию уходит ${formatQuantity(reason.perPortion, reason.unit)} — ${portionsLabel(reason.portionsToClear)} ${reason.portionsToClear * reason.perPortion >= reason.atRisk ? 'закроют остаток' : 'сократят остаток'}.`,
    ]
    if (reason.otherDishes > 0) parts.push(`Продукт идёт ещё в ${plural(reason.otherDishes, 'блюдо', 'блюда', 'блюд')}.`)
    return parts.join(' ')
  }

  // Иконки кнопок карточки: урна, перечёркнутый глаз (скрыть), глаз (показать снова).
  const HOT_ICON = {
    trash: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M19 6l-1 14H6L5 6"/><path d="M10 11v6M14 11v6"/></svg>',
    eyeOff: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94"/><path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19"/><path d="M14.12 14.12a3 3 0 1 1-4.24-4.24"/><path d="M1 1l22 22"/></svg>',
    eye: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/></svg>',
  }

  // «2026-10-15T21:27» для поля datetime-local по часам сервера (в демо они могут идти быстрее).
  const localInput = (ms) => {
    const date = new Date(ms)
    const pad = (n) => String(n).padStart(2, '0')
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
  }

  async function hotView(silent) {
    const me = API.getUser() || {}
    // Официант предлагает на рассмотрение; менеджер, директор и владелец добавляют сразу,
    // рассматривают предложения официантов, скрывают и удаляют предложения.
    const staff = me.role !== 'waiter'
    if (!silent) {
      view.innerHTML = `<main class="page">${pageHeader('Горячие предложения', 'Блюда, продажу которых стоит стимулировать: в них идут продукты, которые иначе не успеют израсходоваться до конца срока.')}
        <div class="form-foot" style="margin-bottom:20px">
          <button type="button" class="btn btn-solid" id="hotAddBtn">Добавить горячее предложение</button>
          <span class="form-msg" id="hotMsg" role="status"></span>
        </div>
        <div id="hotFormWrap"></div>
        <div id="hotBody">${loading()}</div>
      </main>`
      view.querySelector('.viewer-note')?.remove()
      document.getElementById('hotAddBtn').addEventListener('click', openHotForm)
      document.getElementById('hotBody').addEventListener('click', hotAction)
    }
    const body = document.getElementById('hotBody')
    if (!body) return

    let offers
    try {
      offers = await call(API.get('/hot-offers'))
    } catch (error) {
      body.innerHTML = `<div class="glass empty"><h3>Не удалось загрузить предложения</h3><p>${escapeHtml(error.message)}</p></div>`
      return
    }
    if (!document.body.contains(body)) return

    const active = offers.filter((offer) => offer.status === 'active')
    const pending = offers.filter((offer) => offer.status === 'pending')
    const rejected = offers.filter((offer) => offer.status === 'rejected')

    const card = (offer, index) => {
      const manual = offer.source === 'manual'
      return `<article class="glass hot${offer.hidden ? ' is-hidden' : ''}" data-offer="${escapeHtml(offer.id)}" data-name="${escapeHtml(offer.name)}">
        <div class="hot-rank">${index + 1}</div>
        <div class="hot-main">
          <div class="hot-head">
            <h3>${escapeHtml(offer.name)}</h3>
            <span class="label tag" style="margin:0">Цель: ${portionsLabel(offer.target.portions)} до ${formatDateTime(offer.target.until)}</span>
          </div>
          ${manual
            ? `<p class="hot-why">${escapeHtml(offer.note)}</p>
               <p class="hot-also">Добавлено вручную${offer.createdBy ? ': ' + escapeHtml(offer.createdBy) : ''}${offer.hidden ? ' · скрыто от официантов' : ''}</p>`
            : `<p class="hot-why">${reasonText(offer.reasons[0])}</p>
               ${offer.reasons.slice(1).map((reason) => `<p class="hot-also">Заодно: ${reasonText(reason)}</p>`).join('')}
               ${offer.hidden ? '<p class="hot-also">Скрыто от официантов</p>' : ''}`}
        </div>
        <div class="hot-score" title="${manual ? 'Добавлено вручную' : 'Приоритет: срочность × покрытие × эксклюзивность'}">
          ${manual ? '<div class="small">вручную</div>' : `<div class="big">${offer.score}</div>
          <div class="score-track"><i style="width:${offer.score}%"></i></div>
          <div class="small">приоритет</div>`}
          ${staff ? `<div class="hot-actions">
            <button type="button" class="icon-btn" data-hot-hide="${offer.hidden ? 'false' : 'true'}" title="${offer.hidden ? 'Показать официантам' : 'Скрыть от официантов'}" aria-label="${offer.hidden ? 'Показать' : 'Скрыть'}">${offer.hidden ? HOT_ICON.eye : HOT_ICON.eyeOff}</button>
            <button type="button" class="icon-btn" data-hot-delete title="Удалить" aria-label="Удалить">${HOT_ICON.trash}</button>
          </div>` : ''}
        </div>
      </article>`
    }

    // Предложения официантов: менеджер, директор и владелец одобряют или отклоняют,
    // официант видит, что его предложение ждёт решения или отклонено.
    const proposal = (offer) => `<article class="glass hot" data-offer="${escapeHtml(offer.id)}" data-name="${escapeHtml(offer.name)}">
        <div class="hot-rank">·</div>
        <div class="hot-main">
          <div class="hot-head">
            <h3>${escapeHtml(offer.name)}</h3>
            <span class="label tag" style="margin:0">Цель: ${portionsLabel(offer.target.portions)} до ${formatDateTime(offer.target.until)}</span>
          </div>
          <p class="hot-why">${escapeHtml(offer.note)}</p>
          <p class="hot-also">${staff ? 'Предложил официант' + (offer.createdBy ? ' ' + escapeHtml(offer.createdBy) : '') : offer.status === 'rejected' ? 'Менеджер отклонил предложение' : 'Ждёт решения менеджера'}</p>
        </div>
        <div class="hot-score">
          ${staff
            ? `<div class="hot-review"><button type="button" class="btn btn-sm btn-solid" data-hot-review="approve">Одобрить</button><button type="button" class="btn btn-sm btn-line" data-hot-review="reject">Отклонить</button></div>`
            : `<div class="small">${offer.status === 'rejected' ? 'отклонено' : 'на рассмотрении'}</div>`}
        </div>
      </article>`

    const sections = []
    const waiting = staff ? pending : pending.concat(rejected)
    if (waiting.length) {
      sections.push(`<h2 class="panel-title" style="margin:0 0 12px">${staff ? 'На рассмотрении' : 'Мои предложения'}</h2>
        <div class="hot-list" style="margin-bottom:28px">${waiting.map(proposal).join('')}</div>`)
    }
    if (active.length === 0) {
      sections.push(`<div class="glass empty"><h3>Горячих предложений нет</h3><p>Продукты успевают расходоваться до конца срока. Список обновится сам, когда какая-то партия начнёт истекать, — или добавьте предложение вручную.</p></div>`)
    } else {
      sections.push(`<div class="hot-list">${active.map(card).join('')}</div>
        <p class="page-sub" style="margin-top:20px">Приоритет тем выше, чем ближе срок партии, чем большую долю остатка под риском блюдо способно израсходовать и чем меньше других блюд используют этот продукт.</p>`)
    }
    body.innerHTML = sections.join('')
  }

  // Форма ручного предложения: блюдо из меню ресторана, причина, цель и срок.
  async function openHotForm() {
    const wrap = document.getElementById('hotFormWrap')
    if (wrap.firstElementChild) {
      wrap.innerHTML = ''
      return
    }
    const menu = await call(API.get('/menu')).catch(() => [])
    const staff = (API.getUser() || {}).role !== 'waiter'
    wrap.innerHTML = `<form class="glass form" id="hotForm" novalidate style="margin-bottom:24px">
      <h2 class="panel-title">Новое горячее предложение</h2>
      <div class="supply-row" style="grid-template-columns: minmax(0, 1.2fr) minmax(0, 0.5fr) minmax(0, 0.8fr)">
        <label class="field"><span class="label">Блюдо</span><select class="input" name="dishId">
          <option value="">Выберите блюдо из меню</option>
          ${menu.map((dish) => `<option value="${escapeHtml(dish.dishId)}">${escapeHtml(dish.name)}</option>`).join('')}
        </select></label>
        <label class="field"><span class="label">Цель, порций</span><input class="input" name="portions" type="number" min="1" max="10000" value="5" /></label>
        <label class="field"><span class="label">Действует до</span><input class="input" name="until" type="datetime-local" value="${localInput(API.now() + 4 * 3600 * 1000)}" /></label>
      </div>
      <label class="field" style="margin-top:12px"><span class="label">Причина</span>
        <input class="input" name="reason" maxlength="300" placeholder="Например: много томатов, срок подходит" autocomplete="off" /></label>
      <div class="form-foot">
        <button type="submit" class="btn btn-solid">${staff ? 'Добавить' : 'Отправить менеджеру'}</button>
        <button type="button" class="btn btn-line" data-hot-cancel>Отмена</button>
        <span class="form-msg" role="status"></span>
      </div>
    </form>`
    const form = document.getElementById('hotForm')
    const message = form.querySelector('.form-msg')
    form.querySelector('[data-hot-cancel]').addEventListener('click', () => (wrap.innerHTML = ''))
    form.addEventListener('input', (event) => event.target.classList.remove('is-invalid'))
    form.addEventListener('submit', async (event) => {
      event.preventDefault()
      const data = Object.fromEntries(new FormData(form))
      const until = new Date(data.until)
      const bad = {
        dishId: !data.dishId,
        portions: !(Number(data.portions) >= 1),
        until: Number.isNaN(until.getTime()) || until.getTime() <= API.now(),
        reason: !data.reason.trim(),
      }
      Object.entries(bad).forEach(([name, value]) => form.elements[name].classList.toggle('is-invalid', value))
      if (Object.values(bad).some(Boolean)) {
        setMessage(message, 'Выберите блюдо, напишите причину, цель и срок в будущем', true)
        return
      }
      const submit = form.querySelector('[type=submit]')
      submit.disabled = true
      try {
        const result = await call(API.post('/hot-offers', { dishId: data.dishId, reason: data.reason.trim(), portions: Number(data.portions), until: until.toISOString() }))
        wrap.innerHTML = ''
        setMessage(document.getElementById('hotMsg'), result.status === 'pending' ? 'Предложение отправлено менеджеру на рассмотрение' : 'Горячее предложение добавлено')
        hotView(true)
      } catch (error) {
        setMessage(message, error.message, true)
        submit.disabled = false
      }
    })
  }

  // Кнопки на карточках: скрыть / показать, удалить, одобрить / отклонить.
  async function hotAction(event) {
    const button = event.target.closest('[data-hot-hide], [data-hot-delete], [data-hot-review]')
    if (!button) return
    const card = button.closest('[data-offer]')
    const id = encodeURIComponent(card.dataset.offer)
    const message = document.getElementById('hotMsg')
    if (button.hasAttribute('data-hot-delete') && !window.confirm(`Удалить горячее предложение «${card.dataset.name}»?`)) return
    button.disabled = true
    try {
      if (button.hasAttribute('data-hot-delete')) {
        await call(API.del('/hot-offers/' + id))
        setMessage(message, `Предложение «${card.dataset.name}» удалено`)
      } else if (button.hasAttribute('data-hot-review')) {
        const approve = button.dataset.hotReview === 'approve'
        await call(API.post('/hot-offers/' + id + '/review', { action: button.dataset.hotReview }))
        setMessage(message, `Предложение «${card.dataset.name}» ${approve ? 'одобрено' : 'отклонено'}`)
      } else {
        const hidden = button.dataset.hotHide === 'true'
        await call(API.post('/hot-offers/' + id + '/hide', { hidden }))
        setMessage(message, `Предложение «${card.dataset.name}» ${hidden ? 'скрыто от официантов' : 'снова видно официантам'}`)
      }
      await hotView(true)
    } catch (error) {
      setMessage(message, error.message, true)
      button.disabled = false
    }
  }

  /* ---------- Демо: ускорение времени и генератор событий ---------- */

  // Генератор живёт вне экрана, чтобы продолжать работать при переходах по разделам.
  const sim = { timer: null, mode: null, restock: true, accepted: 0, rejected: 0, ticks: 0 }
  const SIM_MODES = { normal: { label: 'Обычный день', every: 3500, maxDishes: 2, maxPortions: 2 }, rush: { label: 'Час пик', every: 1100, maxDishes: 3, maxPortions: 3 } }
  const randomInt = (max) => 1 + Math.floor(Math.random() * max)

  async function simTick() {
    const mode = SIM_MODES[sim.mode]
    if (!mode) return stopSim()
    sim.ticks += 1
    try {
      const menu = await API.get('/menu')
      const available = menu.filter((dish) => !dish.stopped)
      // Пополнение приезжает, когда заметная часть меню ушла в стоп.
      if (sim.restock && menu.length && available.length <= menu.length / 2 && sim.ticks % 4 === 0) await API.post('/demo/restock')
      if (!available.length) return
      const items = new Map()
      for (let index = randomInt(mode.maxDishes); index > 0; index--) {
        const dish = available[Math.floor(Math.random() * available.length)]
        items.set(dish.dishId, randomInt(mode.maxPortions))
      }
      await API.post('/orders', { items: [...items].map(([dishId, portions]) => ({ dishId, portions })) })
      sim.accepted += 1
    } catch (error) {
      if (error.status === 409) sim.rejected += 1
      else if (error.status === 401 || error.status === 403) stopSim()
    }
  }

  function startSim(mode) {
    stopSim()
    sim.mode = mode
    sim.timer = setInterval(simTick, SIM_MODES[mode].every)
  }

  function stopSim() {
    clearInterval(sim.timer)
    sim.timer = null
    sim.mode = null
  }

  const SPEEDS = [[1, 'Реальное время'], [60, '1 мин = 1 ч'], [600, '1 с = 10 мин'], [3600, '1 с = 1 ч']]

  async function demoView(silent) {
    if (!silent) {
      view.innerHTML = `<main class="page">
        ${pageHeader('Демо', 'Ускорение времени и генератор заказов — чтобы за пару минут показать весь путь партии: приёмка, продажи, горячие предложения, списание, стоп-лист.')}
        <div class="demo-grid">
          <section class="glass form">
            <h2 class="panel-title">Время</h2>
            <div><div class="big" id="demoClock">—</div><div class="small" id="demoSpeed"></div></div>
            <div class="chips" id="speedChips" data-admin></div>
            <div class="form-foot" data-admin>
              <button type="button" class="btn btn-line btn-sm" data-advance="1">+1 час</button>
              <button type="button" class="btn btn-line btn-sm" data-advance="6">+6 часов</button>
              <button type="button" class="btn btn-line btn-sm" data-advance="24">+сутки</button>
            </div>
          </section>

          <section class="glass form">
            <h2 class="panel-title">Генератор заказов</h2>
            <div><div class="big" id="simCount">0</div><div class="small" id="simState">остановлен</div></div>
            <div class="form-foot" data-admin>
              <button type="button" class="btn btn-solid btn-sm" data-sim="normal">Обычный день</button>
              <button type="button" class="btn btn-solid btn-sm" data-sim="rush">Час пик</button>
              <button type="button" class="btn btn-line btn-sm" data-sim="stop">Остановить</button>
            </div>
            <label class="check" data-admin><input type="checkbox" id="simRestock" /> Довозить продукты, когда половина меню в стопе</label>
          </section>

          <section class="glass form" data-admin>
            <h2 class="panel-title">Данные</h2>
            <p class="page-sub" style="margin:0">Пример наполнит склад восемью продуктами и меню пятью блюдами. Сброс удалит продукты, поставки, блюда и заказы; аккаунты останутся.</p>
            <div class="form-foot">
              <button type="button" class="btn btn-solid btn-sm" data-demo="seed">Заполнить примером</button>
              <button type="button" class="btn btn-line btn-sm" data-demo="reset" data-confirm="Точно сбросить?">Сбросить данные</button>
            </div>
            <span class="form-msg" id="demoMsg" role="status"></span>
          </section>
        </div>
      </main>`

      document.querySelector('.demo-grid').addEventListener('click', async (event) => {
        const message = document.getElementById('demoMsg')
        const target = event.target.closest('[data-speed], [data-advance], [data-sim], [data-demo]')
        if (!target) return
        try {
          if (target.dataset.sim) target.dataset.sim === 'stop' ? stopSim() : startSim(target.dataset.sim)
          else if (target.dataset.speed) await call(API.post('/demo/clock', { speed: Number(target.dataset.speed) }))
          else if (target.dataset.advance) await call(API.post('/demo/advance', { hours: Number(target.dataset.advance) }))
          else if (target.dataset.demo === 'seed') {
            await call(API.post('/demo/seed'))
            setMessage(message, 'Склад и меню заполнены примером')
          } else if (target.dataset.demo === 'reset') {
            if (!confirmClick(target)) return
            stopSim()
            sim.accepted = sim.rejected = sim.ticks = 0
            await call(API.post('/demo/reset'))
            setMessage(message, 'Данные сброшены, время возвращено к реальному')
          }
        } catch (error) {
          setMessage(message, error.message, true)
        }
        demoView(true)
      })
      document.getElementById('simRestock').addEventListener('change', (event) => {
        sim.restock = event.target.checked
      })
    }

    const chips = document.getElementById('speedChips')
    if (!chips) return
    let state
    try {
      state = await call(API.get('/demo/state'))
    } catch {
      return
    }
    if (!document.body.contains(chips)) return

    chips.innerHTML = SPEEDS.map(([speed, label]) => `<button type="button" class="chip${speed === state.speed ? ' is-on' : ''}" data-speed="${speed}">${label}</button>`).join('')
    document.getElementById('demoSpeed').textContent = state.shifted ? (state.speed === 1 ? 'время перемотано, идёт с обычной скоростью' : 'ускорено в ' + state.speed + ' раз') : 'реальное время'
    document.getElementById('demoClock').textContent = formatDateTime(API.now())
    document.getElementById('simRestock').checked = sim.restock
    document.getElementById('simCount').textContent = sim.accepted + sim.rejected
    document.getElementById('simState').textContent = sim.mode
      ? `${SIM_MODES[sim.mode].label}: оформлено ${sim.accepted}, отклонено ${sim.rejected}`
      : sim.accepted + sim.rejected > 0
        ? `остановлен: оформлено ${sim.accepted}, отклонено ${sim.rejected}`
        : 'остановлен'
  }

  /* ---------- Автообновление и всплывающие уведомления ---------- */

  let refreshView = null // как тихо обновить открытый экран, не сбрасывая введённое
  let seenRev = API.rev()
  let toastsReady = false
  const shownAlerts = new Set()

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

  // Раз в несколько секунд сверяемся с сервером: если данные изменились (прошло время,
  // сработал генератор, поработал другой администратор) — тихо обновляем открытый экран.
  // С настоящим бэкендом вместо опроса сюда подключается поток событий (SSE).
  async function pulse() {
    try {
      const state = await API.get('/pulse')
      const chip = document.getElementById('clockChip')
      if (chip) {
        chip.hidden = state.speed === 1
        chip.textContent = '×' + state.speed + ' · ' + formatDateTime(state.now)
      }
      showToasts(state.alerts)
      if (state.rev !== seenRev) {
        seenRev = state.rev
        if (refreshView) refreshView()
      } else if (state.speed > 1 && refreshView && document.getElementById('demoClock')) refreshView()
    } catch {
      // Нет связи — попробуем на следующем тике.
    }
  }
  setInterval(pulse, 3000)

  /* ---------- Экран для неизвестного адреса ---------- */

  const PLANNED = {
  }

  function placeholderView(path) {
    const [title, plannedFor] = PLANNED[path] || ['Страница не найдена', null]
    view.innerHTML = `<main class="page">${pageHeader(title)}<div class="glass empty"><p>${plannedFor ? 'Экран в работе, по плану — ' + plannedFor : 'Такой страницы нет. Выберите раздел в меню.'}</p></div></main>`
  }

  /* ---------- Маршруты ---------- */

  const PUBLIC = ['', 'login', 'register']
  let cleanup = null

  // Экран только для чтения: открываем и запоминаем, как обновить его без мигания.
  function open(screen) {
    screen()
    refreshView = () => screen(true)
  }

  function route() {
    const [rawPath, rawQuery] = location.hash.replace(/^#\/?/, '').split('?')
    const path = rawPath.replace(/\/$/, '')
    const query = new URLSearchParams(rawQuery || '')

    // Главная и вход — отдельные страницы.
    if (path === '') {
      location.replace('index.html')
      return
    }
    // Официанту из экранов этой страницы доступно только «Горячее».
    if ((API.getUser() || {}).role === 'waiter' && !['hot-offers', 'login', 'register'].includes(path) && !PAGES[path]) {
      location.replace('orders.html')
      return
    }
    // Владелец сначала выбирает ресторан.
    if (isOwner() && !API.getRestaurant() && !['restaurants', 'admin', 'login', 'register'].includes(path)) {
      location.replace('#/restaurants')
      return
    }
    // Экран переехал в отдельный файл — отправляем туда, сохранив параметры.
    if (PAGES[path]) {
      location.replace(PAGES[path] + (rawQuery ? '?' + rawQuery : ''))
      return
    }

    if (cleanup) cleanup()
    cleanup = null
    refreshView = null
    setMenu(false)
    app.className = PUBLIC.includes(path) ? 'app is-home' : 'app'
    // Менеджер видит все данные, но кнопки изменения у него скрыты.
    if (API.getUser() && !isAdmin()) app.classList.add('is-viewer')
    renderNav(path)
    window.scrollTo(0, 0)

    if (path === 'login' || path === 'register') authView(path, query)
    else if (path === 'menu') menuView()
    else if (path === 'hot-offers') open(hotView)
    else if (path === 'expiry') {
      const filter = query.get('filter')
      expiryFilter = ['ok', 'warning', 'critical', 'written'].includes(filter) ? filter : 'all'
      open(expiryView)
    }
    else if (path === 'demo') open(demoView)
    else if (path === 'admin') adminView()
    else if (path === 'restaurants') restaurantsView()
    else placeholderView(path)
    pulse()
  }

  window.addEventListener('hashchange', route)
  route()
})()
