/* Заявка заказчика на банкет: блюда меню и число порций. Сервер распознаёт документ,
   сопоставляет блюда с меню, берёт цены порций из БД и считает итог. */
;(() => {
  if (!UI.shell('requests')) return

  const el = (id) => document.getElementById(id)
  const uploadForm = el('requestUploadForm')
  const fileInput = el('requestFile')
  const dropzone = el('requestDropzone')
  const fileName = el('fileName')
  const analyzeButton = el('analyzeButton')
  const uploadMessage = el('uploadMessage')
  const aiStatus = el('aiStatus')
  const aiStatusTitle = el('aiStatusTitle')
  const aiStatusText = el('aiStatusText')
  const warningsBox = el('requestWarnings')
  const editor = el('requestEditor')
  const customerName = el('customerName')
  const sourceFilename = el('sourceFilename')
  const sourceHashShort = el('sourceHashShort')
  const rowsNode = el('requestItems')
  const stockNode = el('requestStock')
  const globalDiscountNode = el('globalDiscount')
  const saveButton = el('saveRequest')
  const saveMessage = el('saveMessage')
  const catalogNode = el('catalogPrices')
  const historyNode = el('requestHistory')
  const admin = UI.isAdmin()

  const money = new Intl.NumberFormat('ru-RU', { style: 'currency', currency: 'RUB', maximumFractionDigits: 2 })
  let menu = []
  let items = []
  let globalDiscount = 0
  let currentSourceFilename = ''
  let currentSourceHash = ''
  let aiUsed = false
  let duplicateSaved = false

  function message(node, text, isError) {
    UI.setMessage(node, text, Boolean(isError))
  }

  function rubles(kopecks) {
    return money.format((Number(kopecks) || 0) / 100)
  }

  function rawMoney(kopecks) {
    return (Number(kopecks || 0) / 100).toFixed(2)
  }

  function manualPriceKopecks(value) {
    if (value === null || value === undefined || String(value).trim() === '') return null
    const parsed = Number(String(value).replace(',', '.'))
    if (!Number.isFinite(parsed) || parsed < 0 || parsed > 10000000000) return null
    return Math.round(parsed * 100)
  }

  function dishByID(id) {
    return menu.find((dish) => String(dish.dishId) === String(id)) || null
  }

  function portionsValue(value) {
    const parsed = Math.round(Number(String(value ?? '').replace(',', '.')))
    return Number.isFinite(parsed) && parsed > 0 && parsed <= 100000 ? parsed : 0
  }

  function discountValue(value) {
    const parsed = Number(value)
    return Number.isFinite(parsed) ? Math.max(0, Math.min(100, Math.round(parsed))) : 0
  }

  function setMenu(newMenu) {
    menu = Array.isArray(newMenu) ? newMenu : []
    renderPrices()
    if (items.length) renderRows()
  }

  async function loadMenu() {
    try {
      setMenu(await API.get('/menu'))
    } catch (error) {
      catalogNode.innerHTML = '<tr><td colspan="3" class="request-error">Не удалось загрузить меню: ' + UI.escapeHtml(error.message) + '</td></tr>'
    }
  }

  function renderWarnings(warnings) {
    const values = Array.isArray(warnings) ? warnings.filter(Boolean) : []
    warningsBox.hidden = values.length === 0
    warningsBox.textContent = values.join(' ')
  }

  function updateAIStatus(title, text, warning) {
    aiStatus.hidden = false
    aiStatusTitle.textContent = title
    aiStatusText.textContent = text
    aiStatus.classList.toggle('is-warning', Boolean(warning))
  }

  function handlePickedFile(file) {
    if (!file) return
    fileName.textContent = file.name
    uploadMessage.textContent = ''
    uploadMessage.className = 'form-msg'
    dropzone.classList.remove('is-dragging')
  }

  fileInput.addEventListener('change', () => handlePickedFile(fileInput.files[0]))
  ;['dragenter', 'dragover'].forEach((eventName) => dropzone.addEventListener(eventName, (event) => {
    event.preventDefault()
    dropzone.classList.add('is-dragging')
  }))
  ;['dragleave', 'drop'].forEach((eventName) => dropzone.addEventListener(eventName, (event) => {
    event.preventDefault()
    dropzone.classList.remove('is-dragging')
  }))
  dropzone.addEventListener('drop', (event) => {
    const file = event.dataTransfer && event.dataTransfer.files && event.dataTransfer.files[0]
    if (!file) return
    try {
      const transfer = new DataTransfer()
      transfer.items.add(file)
      fileInput.files = transfer.files
      handlePickedFile(file)
    } catch {
      fileName.textContent = file.name + ' — выбери этот файл через диалог'
    }
  })

  el('createManualRequest').addEventListener('click', () => {
    if (items.length && !window.confirm('Заменить текущий черновик пустой заявкой?')) return
    items = []
    currentSourceFilename = ''
    currentSourceHash = ''
    duplicateSaved = false
    customerName.value = ''
    sourceFilename.textContent = 'Заявка без файла'
    sourceHashShort.textContent = 'Создана вручную'
    globalDiscount = 0
    globalDiscountNode.value = '0'
    aiStatus.hidden = true
    renderWarnings([])
    editor.hidden = false
    renderRows()
    message(uploadMessage, 'Пустой черновик создан. Добавь блюда из меню и число порций.', false)
    editor.scrollIntoView({ behavior: 'smooth', block: 'start' })
  })

  uploadForm.addEventListener('submit', async (event) => {
    event.preventDefault()
    const file = fileInput.files[0]
    if (!file) {
      message(uploadMessage, 'Сначала выбери файл заявки.', true)
      return
    }
    if (file.size > 12 * 1024 * 1024) {
      message(uploadMessage, 'Файл больше 12 МБ.', true)
      return
    }
    analyzeButton.disabled = true
    analyzeButton.textContent = 'Анализируем…'
    message(uploadMessage, 'Распознаём заявку и ищем блюда в меню…', false)
    renderWarnings([])
    try {
      const result = await API.upload('/requests/parse', file).then(API.awaitJob)
      currentSourceFilename = result.sourceFilename || file.name
      currentSourceHash = result.sourceHash || ''
      aiUsed = Boolean(result.aiUsed)
      duplicateSaved = Boolean(result.duplicateSaved)
      customerName.value = result.customerName || ''
      sourceFilename.textContent = currentSourceFilename
      sourceHashShort.textContent = currentSourceHash ? 'SHA-256 · ' + currentSourceHash.slice(0, 16) + '…' : ''
      if (Array.isArray(result.catalog)) setMenu(result.catalog)
      items = (Array.isArray(result.items) ? result.items : []).map((item) => ({
        originalName: item.originalName || item.dishName || '',
        dishId: item.dishId || '',
        portions: item.portions || 1,
        catalogMatch: Boolean(item.catalogMatch),
        manualPrice: '',
        itemDiscountPercent: Number(item.itemDiscountPercent || 0),
      }))
      globalDiscount = 0
      globalDiscountNode.value = '0'
      editor.hidden = false
      renderRows()
      const parserMode = result.parserMode || (aiUsed ? 'ai' : 'fallback')
      updateAIStatus(
        parserMode === 'ai' ? 'Нейросеть обработала заявку' : parserMode === 'structured' ? 'Список распознан по строкам' : 'Включён упрощённый разбор',
        parserMode === 'ai'
          ? 'Qwen выделила ' + items.length + ' поз. Проверь блюда и число порций.'
          : parserMode === 'structured'
            ? 'Простой список разобран без ожидания модели. Проверь блюда и число порций.'
            : 'Ollama не ответила или не выделила блюда. Использован упрощённый разбор; проверь распознавание.',
        parserMode === 'fallback'
      )
      const warnings = [...(result.warnings || [])]
      if (duplicateSaved) warnings.unshift('Этот файл уже встречался в сохранённых заявках. Повторное сохранение будет заблокировано.')
      renderWarnings(warnings)
      message(uploadMessage, items.length ? 'Черновик готов к проверке.' : 'Файл прочитан. Добавь блюда вручную.', false)
      editor.scrollIntoView({ behavior: 'smooth', block: 'start' })
    } catch (error) {
      message(uploadMessage, error.message || 'Не удалось обработать файл.', true)
    } finally {
      analyzeButton.disabled = false
      analyzeButton.textContent = 'Распознать документ'
    }
  })

  function renderRows() {
    if (!items.length) {
      rowsNode.innerHTML = '<tr><td colspan="6" class="request-empty">Пока нет позиций. Нажми «Добавить блюдо» и выбери блюдо из меню.</td></tr>'
      updateTotals()
      return
    }
    rowsNode.innerHTML = items.map((item, index) => {
      const dish = dishByID(item.dishId)
      const optionList = ['<option value="">Выбери блюдо…</option>'].concat(menu.map((option) =>
        '<option value="' + UI.escapeHtml(option.dishId) + '"' + (String(option.dishId) === String(item.dishId) ? ' selected' : '') + '>' + UI.escapeHtml(option.name) + '</option>'
      )).join('')

      let priceText = '<span class="request-not-set">Сначала выбери блюдо</span>'
      if (dish && dish.priceConfigured) {
        priceText = '<span class="request-price">' + rubles(dish.priceKopecks) + '</span><span class="request-unit-price-note">за порцию · из меню</span>'
      } else if (dish) {
        priceText = '<div class="request-manual-price"><input class="input" data-item-price="' + index + '" type="number" min="0" max="10000000000" step="0.01" inputmode="decimal" value="' + UI.escapeHtml(item.manualPrice || '') + '" placeholder="Цена порции" aria-label="Цена порции, позиция ' + (index + 1) + '" /><span>₽</span></div><span class="request-unit-price-note request-not-set">Цена порции не задана в меню — только для этой заявки</span>'
      }

      const notes = []
      if (!dish && item.originalName && !item.catalogMatch) {
        notes.push('Блюда «' + item.originalName + '» нет в меню. Выбери замену или удали строку.')
      } else if (!dish) {
        notes.push('Выбери блюдо из меню.')
      } else {
        if (item.originalName && item.originalName.toLowerCase() !== dish.name.toLowerCase()) notes.push('В заявке: ' + item.originalName)
        notes.push(dish.stopped ? 'Сейчас в стоп-листе — продуктов не хватает даже на одну порцию.' : 'Из остатков можно приготовить сейчас: ' + UI.portionsLabel(dish.availablePortions))
      }
      const sourceNote = notes.map((note) => '<span class="request-original-name">' + UI.escapeHtml(note) + '</span>').join('')
      return '<tr data-request-row="' + index + '">' +
        '<td class="request-product-cell"><select class="input" data-dish-select="' + index + '" aria-label="Блюдо позиции ' + (index + 1) + '">' + optionList + '</select>' + sourceNote + '</td>' +
        '<td><input class="input" data-portions="' + index + '" type="number" min="1" max="100000" step="1" inputmode="numeric" value="' + UI.escapeHtml(item.portions) + '" aria-label="Порций, позиция ' + (index + 1) + '" /><span class="request-unit-price-note">порц.</span></td>' +
        '<td>' + priceText + '</td>' +
        '<td><div class="request-discount-input"><input class="input" data-item-discount="' + index + '" type="number" min="0" max="100" step="1" value="' + discountValue(item.itemDiscountPercent) + '" aria-label="Скидка позиции в процентах" /><span>%</span></div></td>' +
        '<td class="request-line-total" data-row-total="' + index + '">—</td>' +
        '<td><button type="button" class="request-remove" data-remove-item="' + index + '" aria-label="Удалить позицию">×</button></td>' +
        '</tr>'
    }).join('')
    updateTotals()
  }

  function updateTotals() {
    let subtotal = 0
    let total = 0
    let missingDish = false
    let missingPrice = false
    let invalidPortions = false

    items.forEach((item, index) => {
      const dish = dishByID(item.dishId)
      const portions = portionsValue(item.portions)
      const cell = rowsNode.querySelector('[data-row-total="' + index + '"]')
      if (!dish) {
        missingDish = true
        if (cell) cell.textContent = item.originalName ? 'Нет в меню' : 'Выбери блюдо'
        return
      }
      if (!portions) {
        invalidPortions = true
        if (cell) cell.textContent = 'Укажи порции'
        return
      }
      const unitPriceKopecks = dish.priceConfigured ? Number(dish.priceKopecks || 0) : manualPriceKopecks(item.manualPrice)
      if (unitPriceKopecks === null) {
        missingPrice = true
        if (cell) cell.textContent = 'Введи цену'
        return
      }
      const lineSubtotal = unitPriceKopecks * portions
      const lineTotal = Math.round(lineSubtotal * (1 - discountValue(item.itemDiscountPercent) / 100) * (1 - globalDiscount / 100))
      subtotal += lineSubtotal
      total += lineTotal
      if (cell) cell.textContent = rubles(lineTotal)
    })

    el('summarySubtotal').textContent = rubles(subtotal)
    el('summaryDiscount').textContent = '−' + rubles(Math.max(0, subtotal - total))
    el('summaryTotal').textContent = rubles(total)

    const canSave = admin && items.length > 0 && !missingDish && !missingPrice && !invalidPortions && !duplicateSaved
    saveButton.disabled = !canSave
    if (!admin) message(saveMessage, 'Сохранять заявки и менять цены могут владелец и директор ресторана.', true)
    else if (duplicateSaved) message(saveMessage, 'Этот исходный файл уже сохранён. Загрузи другой файл или создай заявку вручную.', true)
    else if (missingDish) message(saveMessage, 'Для каждой позиции выбери блюдо из меню. Если его нет — добавь блюдо во вкладке «Меню» или удали строку.', true)
    else if (missingPrice) message(saveMessage, 'Для блюда без цены порции введи цену вручную. Она сохранится только в этой заявке.', true)
    else if (invalidPortions) message(saveMessage, 'Укажи число порций от 1 до 100 000 для каждой позиции.', true)
    else message(saveMessage, '', false)
    scheduleStockCheck()
  }

  /* ---------- Хватит ли продуктов на все порции ---------- */

  // Проверка идёт через тот же расчёт, что у заказов, но ничего не списывает: потребность
  // складывается по всем блюдам заявки сразу.
  let stockTimer = 0
  let stockRequest = 0
  function scheduleStockCheck() {
    clearTimeout(stockTimer)
    stockTimer = setTimeout(checkStock, 350)
  }

  async function checkStock() {
    const order = items
      .map((item) => ({ dishId: String(item.dishId || ''), portions: portionsValue(item.portions) }))
      .filter((item) => item.dishId && item.portions)
    if (!order.length) {
      stockNode.hidden = true
      return
    }
    const ticket = ++stockRequest
    try {
      const result = await API.post('/orders/validate', { items: order })
      if (ticket !== stockRequest) return
      stockNode.hidden = false
      if (result.ok) {
        stockNode.className = 'request-stock is-ok'
        stockNode.innerHTML = '<strong>Продуктов на складе хватает на все порции.</strong><span>Склад при сохранении заявки не меняется — продукты спишутся, когда заказ оформят во вкладке «Заказы».</span>'
        return
      }
      stockNode.className = 'request-stock is-short'
      stockNode.innerHTML = '<strong>На все порции не хватит продуктов — нужно докупить:</strong><ul>' +
        result.shortages.map((item) => '<li><b>' + UI.escapeHtml(item.productName) + '</b> — нужно ' + UI.formatQuantity(item.required, item.unit) +
          ', есть ' + UI.formatQuantity(item.available, item.unit) + ', не хватает <b>' + UI.formatQuantity(item.missing, item.unit) + '</b>' +
          (item.dishes && item.dishes.length ? ' <span class="request-muted">(' + UI.escapeHtml(item.dishes.join(', ')) + ')</span>' : '') + '</li>').join('') +
        '</ul><span>Заявку можно сохранить и так — это предложение заказчику, склад не меняется.</span>'
    } catch (error) {
      if (ticket !== stockRequest) return
      stockNode.hidden = false
      stockNode.className = 'request-stock is-short'
      stockNode.textContent = 'Не удалось проверить остатки: ' + error.message
    }
  }

  rowsNode.addEventListener('change', (event) => {
    const dishSelect = event.target.closest('[data-dish-select]')
    if (!dishSelect) return
    const item = items[Number(dishSelect.dataset.dishSelect)]
    if (!item) return
    item.dishId = dishSelect.value
    item.catalogMatch = Boolean(dishSelect.value)
    item.manualPrice = ''
    renderRows()
  })

  rowsNode.addEventListener('input', (event) => {
    const portionsInput = event.target.closest('[data-portions]')
    const discountInput = event.target.closest('[data-item-discount]')
    const priceInput = event.target.closest('[data-item-price]')
    if (priceInput && items[Number(priceInput.dataset.itemPrice)]) items[Number(priceInput.dataset.itemPrice)].manualPrice = priceInput.value
    if (portionsInput && items[Number(portionsInput.dataset.portions)]) items[Number(portionsInput.dataset.portions)].portions = portionsInput.value
    if (discountInput && items[Number(discountInput.dataset.itemDiscount)]) items[Number(discountInput.dataset.itemDiscount)].itemDiscountPercent = discountValue(discountInput.value)
    if (portionsInput || discountInput || priceInput) updateTotals()
  })

  rowsNode.addEventListener('click', (event) => {
    const remove = event.target.closest('[data-remove-item]')
    if (!remove) return
    items.splice(Number(remove.dataset.removeItem), 1)
    renderRows()
  })

  el('addRequestItem').addEventListener('click', () => {
    items.push({ originalName: '', dishId: '', portions: 10, catalogMatch: false, manualPrice: '', itemDiscountPercent: 0 })
    renderRows()
    const select = rowsNode.querySelector('[data-dish-select="' + (items.length - 1) + '"]')
    if (select) select.focus()
  })

  globalDiscountNode.addEventListener('input', () => {
    globalDiscount = discountValue(globalDiscountNode.value)
    updateTotals()
  })

  saveButton.addEventListener('click', async () => {
    if (saveButton.disabled) return
    const payload = {
      customerName: customerName.value.trim(),
      sourceFilename: currentSourceFilename,
      sourceHash: currentSourceHash,
      globalDiscountPercent: globalDiscount,
      items: items.map((item) => {
        const row = { dishId: String(item.dishId), portions: portionsValue(item.portions), itemDiscountPercent: discountValue(item.itemDiscountPercent) }
        const dish = dishByID(item.dishId)
        if (dish && !dish.priceConfigured) row.unitPriceKopecks = manualPriceKopecks(item.manualPrice)
        return row
      }),
    }
    saveButton.disabled = true
    saveButton.textContent = 'Сохраняем…'
    try {
      const saved = await API.post('/requests', payload)
      message(saveMessage, 'Заявка ' + saved.number + ' сохранена. Итого: ' + rubles(saved.totalKopecks) + '.', false)
      items = []
      currentSourceFilename = ''
      currentSourceHash = ''
      globalDiscount = 0
      globalDiscountNode.value = '0'
      editor.hidden = true
      stockNode.hidden = true
      duplicateSaved = false
      renderWarnings([])
      message(uploadMessage, 'Заявка сохранена: ' + saved.number, false)
      await loadHistory()
    } catch (error) {
      message(saveMessage, error.message || 'Не удалось сохранить заявку.', true)
    } finally {
      saveButton.textContent = 'Сохранить заявку'
      updateTotals()
    }
  })

  /* ---------- Цены порций ---------- */

  async function saveDishPrice(dishID) {
    const input = catalogNode.querySelector('[data-price-input="' + dishID + '"]')
    if (!input) return
    const value = Number(String(input.value).replace(',', '.'))
    if (!Number.isFinite(value) || value < 0 || value > 10000000000) {
      input.setCustomValidity('Укажи цену от 0 до 10 000 000 000 ₽')
      input.reportValidity()
      input.setCustomValidity('')
      return
    }
    const button = catalogNode.querySelector('[data-save-price="' + dishID + '"]')
    if (button) {
      button.disabled = true
      button.textContent = '…'
    }
    try {
      await API.post('/dishes/' + encodeURIComponent(dishID) + '/price', { priceKopecks: Math.round(value * 100) })
      await loadMenu()
    } catch (error) {
      if (button) {
        button.disabled = false
        button.textContent = 'Сохранить'
      }
      const row = catalogNode.querySelector('[data-price-row="' + dishID + '"]')
      if (row) {
        let warning = row.querySelector('.price-error')
        if (!warning) {
          warning = document.createElement('span')
          warning.className = 'price-error request-error'
          row.querySelector('td:last-child').append(warning)
        }
        warning.textContent = error.message
      }
    }
  }

  function renderPrices() {
    if (!menu.length) {
      catalogNode.innerHTML = '<tr><td colspan="3" class="request-muted">Меню пока пустое. Добавь блюда во вкладке «Меню».</td></tr>'
      return
    }
    catalogNode.innerHTML = menu.map((dish) => {
      const id = UI.escapeHtml(dish.dishId)
      return '<tr data-price-row="' + id + '">' +
        '<td>' + UI.escapeHtml(dish.name) + (dish.priceConfigured ? '' : '<span class="request-unit-price-note request-not-set">Цена не задана</span>') + '</td>' +
        '<td><div class="price-edit"><input class="input" data-price-input="' + id + '" type="number" min="0" max="10000000000" step="0.01" value="' + (dish.priceConfigured ? rawMoney(dish.priceKopecks) : '') + '" placeholder="0,00" ' + (!admin ? 'disabled' : '') + ' aria-label="Цена порции: ' + UI.escapeHtml(dish.name) + '" /><span>₽</span></div></td>' +
        '<td><button type="button" class="btn btn-line btn-sm" data-save-price="' + id + '"' + (!admin ? ' disabled' : '') + '>Сохранить</button></td>' +
        '</tr>'
    }).join('')
  }

  catalogNode.addEventListener('click', (event) => {
    const button = event.target.closest('[data-save-price]')
    if (button) saveDishPrice(button.dataset.savePrice)
  })

  /* ---------- История ---------- */

  async function loadHistory() {
    try {
      const requests = await API.get('/requests')
      if (!Array.isArray(requests) || requests.length === 0) {
        historyNode.innerHTML = '<div class="request-empty">Сохранённых заявок пока нет. После нажатия «Сохранить заявку» они появятся здесь.</div>'
        return
      }
      historyNode.innerHTML = requests.map((request) => {
        const created = request.createdAt ? new Date(request.createdAt).toLocaleString('ru-RU') : '—'
        const lines = Array.isArray(request.items) ? request.items : []
        const portions = lines.reduce((sum, line) => sum + Number(line.portions || 0), 0)
        return '<details class="request-history-card"><summary>' +
          '<span class="request-history-number">' + UI.escapeHtml(request.number) + '</span>' +
          '<span class="request-history-customer">' + UI.escapeHtml(request.customerName) + '</span>' +
          '<span class="request-muted">' + UI.escapeHtml(created) + '</span>' +
          '<span class="request-status-label">Сохранена</span>' +
          '<strong class="request-history-total">' + rubles(request.totalKopecks) + '</strong>' +
          '</summary><div class="request-history-detail">' +
          '<div class="request-history-meta">Блюд: ' + lines.length + ' · Порций: ' + portions + ' · Скидка: ' + rubles(request.discountKopecks) + (request.sourceFilename ? ' · Файл: ' + UI.escapeHtml(request.sourceFilename) : '') + '</div>' +
          '<div class="request-table-wrap"><table class="request-history-table"><thead><tr><th>Блюдо</th><th>Порций</th><th>Цена порции</th><th>Скидка</th><th>Сумма</th></tr></thead><tbody>' +
          lines.map((line) => '<tr><td>' + UI.escapeHtml(line.dishName) + '</td><td>' + Number(line.portions || 0) + '</td><td>' + rubles(line.unitPriceKopecks) + '</td><td>' + Number(line.itemDiscountPercent || 0) + '%</td><td class="request-history-total">' + rubles(line.totalKopecks) + '</td></tr>').join('') +
          '</tbody></table></div></div></details>'
      }).join('')
    } catch (error) {
      historyNode.innerHTML = '<div class="request-empty request-error">Не удалось загрузить историю: ' + UI.escapeHtml(error.message) + '</div>'
    }
  }

  el('refreshRequests').addEventListener('click', loadHistory)

  async function init() {
    if (!admin) updateAIStatus('Режим просмотра', 'Заявки и цены могут изменять владелец и директор ресторана.', true)
    await Promise.all([loadMenu(), loadHistory()])
  }

  init()
})();
