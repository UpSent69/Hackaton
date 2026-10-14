/* Плавные переходы на всём сайте, в духе видео-референса.
   — Между вкладками (Склад → Поставки → … → Демо) страница перелистывается: на вкладку правее
     содержимое уезжает влево и новое въезжает справа, на вкладку левее — наоборот. Шапка с
     навигацией стоит на месте, листается только содержимое.
   — Остальные переходы (главная, вход) — мягкое растворение с лёгким блюром.
   В браузерах с View Transitions переход рисует сам браузер; в остальных — запасная анимация
   с тем же направлением. Подключается в <head> каждой страницы, чтобы стили и направление
   были на месте до первой отрисовки. */
;(() => {
  const EASE_OUT = 'cubic-bezier(0.16, 1, 0.3, 1)'
  const EASE = 'cubic-bezier(0.4, 0, 0.2, 1)'
  const html = document.documentElement
  const supportsVT = typeof CSS !== 'undefined' && CSS.supports && CSS.supports('view-transition-name: none')
  const sameDocVT = typeof document.startViewTransition === 'function'
  const reduced = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches

  /* ---------- Порядок вкладок — как в навигации, слева направо ---------- */

  const ORDER = ['stock', 'supplies', 'requests', 'menu', 'orders', 'stoplist', 'hot-offers', 'expiry', 'analytics', 'demo']
  const FILES = {
    'sclad.html': 'stock', 'supplies.html': 'supplies', 'requests.html': 'requests',
    'orders.html': 'orders', 'stoplist.html': 'stoplist', 'analytics.html': 'analytics',
  }

  // Вкладка по адресу: отдельный файл или экран внутри app.html (#/menu, #/expiry?filter=…).
  function tabOf(href) {
    let url
    try { url = new URL(href, location.href) } catch { return null }
    if (url.origin !== location.origin) return null
    const file = url.pathname.split('/').pop() || 'index.html'
    if (FILES[file]) return FILES[file]
    if (file === 'app.html') {
      const route = url.hash.replace(/^#\/?/, '').split('?')[0].replace(/\/$/, '')
      return ORDER.includes(route) ? route : null
    }
    return null
  }

  // 'next' — вкладка правее, 'prev' — левее, '' — не между вкладками (обычное растворение).
  function directionOf(fromHref, toHref) {
    const from = ORDER.indexOf(tabOf(fromHref))
    const to = ORDER.indexOf(tabOf(toHref))
    if (from < 0 || to < 0 || from === to) return ''
    return to > from ? 'next' : 'prev'
  }

  // Направление передаётся новой странице через sessionStorage: её стили рисуют и уход старой.
  const KEY = 'mx.dir'
  function rememberDirection(dir, toHref) {
    try { sessionStorage.setItem(KEY, JSON.stringify({ dir, to: new URL(toHref, location.href).href, at: Date.now() })) } catch {}
  }
  function takeDirection() {
    try {
      const saved = JSON.parse(sessionStorage.getItem(KEY) || 'null')
      sessionStorage.removeItem(KEY)
      // Только если пришли именно туда, куда шли: перезагрузка или ввод адреса — без направления.
      if (saved && saved.to === location.href && Date.now() - saved.at < 5000) return saved.dir || ''
    } catch {}
    return ''
  }
  function setDirection(dir) {
    html.classList.remove('mx-next', 'mx-prev')
    if (dir) html.classList.add('mx-' + dir)
  }

  /* ---------- Стили ---------- */

  const OUT = `0.5s ${EASE}`
  const IN = `0.75s ${EASE_OUT}`
  // Перелистывание: старая и новая страницы едут одним листом — одинаковая длительность и
  // плавный разгон с мягкой остановкой.
  const SLIDE = `0.9s cubic-bezier(0.65, 0, 0.35, 1)`
  const style = document.createElement('style')
  style.textContent = `
    @view-transition { navigation: auto; }

    /* Шапка не листается вместе со страницей и не меняет размер по ходу перехода
       (у склада она ниже, чем у остальных страниц) — просто стоит на месте */
    .nav { view-transition-name: mx-nav; }
    ::view-transition-group(mx-nav), ::view-transition-new(mx-nav) { animation: none; }
    ::view-transition-old(mx-nav) { animation: none; opacity: 0; }

    /* Обычный переход: растворение с лёгким блюром */
    ::view-transition-old(root) { animation: mxOut ${OUT} both; }
    ::view-transition-new(root) { animation: mxIn ${IN} both; }

    /* Перелистывание между вкладками */
    html.mx-next::view-transition-old(root) { animation: mxToLeft ${SLIDE} both; }
    html.mx-next::view-transition-new(root) { animation: mxFromRight ${SLIDE} both; }
    html.mx-prev::view-transition-old(root) { animation: mxToRight ${SLIDE} both; }
    html.mx-prev::view-transition-new(root) { animation: mxFromLeft ${SLIDE} both; }

    @keyframes mxOut { to { opacity: 0; filter: blur(6px); } }
    @keyframes mxIn { from { opacity: 0; filter: blur(8px); } }
    /* Сдвиг строго по горизонтали: вертикальная координата всегда 0 */
    @keyframes mxToLeft {
      from { opacity: 1; filter: blur(0); transform: translate3d(0, 0, 0); }
      to { opacity: 0; filter: blur(3px); transform: translate3d(-12vw, 0, 0); }
    }
    @keyframes mxToRight {
      from { opacity: 1; filter: blur(0); transform: translate3d(0, 0, 0); }
      to { opacity: 0; filter: blur(3px); transform: translate3d(12vw, 0, 0); }
    }
    @keyframes mxFromRight {
      from { opacity: 0; filter: blur(3px); transform: translate3d(12vw, 0, 0); }
      to { opacity: 1; filter: blur(0); transform: translate3d(0, 0, 0); }
    }
    @keyframes mxFromLeft {
      from { opacity: 0; filter: blur(3px); transform: translate3d(-12vw, 0, 0); }
      to { opacity: 1; filter: blur(0); transform: translate3d(0, 0, 0); }
    }

    ${supportsVT ? '' : `
    /* Запасной вариант без View Transitions: анимируется сама страница */
    body { animation: mxIn ${IN} both; }
    html.mx-next body { animation: mxFromRight ${SLIDE} both; }
    html.mx-prev body { animation: mxFromLeft ${SLIDE} both; }
    html.mx-leave body { animation: mxOut 0.3s ${EASE} both; }
    html.mx-leave.mx-next body { animation: mxToLeft 0.3s ${EASE} both; }
    html.mx-leave.mx-prev body { animation: mxToRight 0.3s ${EASE} both; }`}

    /* Экраны внутри app.html без View Transitions */
    .mx-view { animation: mxIn ${IN} both; }
    html.mx-next .mx-view { animation: mxFromRight ${SLIDE} both; }
    html.mx-prev .mx-view { animation: mxFromLeft ${SLIDE} both; }

    @media (prefers-reduced-motion: reduce) {
      ::view-transition-group(*), ::view-transition-old(*), ::view-transition-new(*),
      body, html.mx-leave body, .mx-view { animation: none !important; }
    }
  `
  document.head.appendChild(style)

  /* ---------- Переходы между страницами ---------- */

  // Старая страница: направление по адресу назначения (переход по ссылке, по кнопке, из скрипта).
  window.addEventListener('pageswap', (event) => {
    const to = event.activation && event.activation.entry && event.activation.entry.url
    if (to) rememberDirection(directionOf(location.href, to), to)
  })

  // Новая страница: направление выставляется до первой отрисовки, чтобы им была нарисована
  // и уходящая страница; после перехода класс снимается.
  const arrived = takeDirection()
  setDirection(arrived)
  window.addEventListener('pagereveal', (event) => {
    if (event.viewTransition) event.viewTransition.finished.finally(() => setDirection(''))
    else if (supportsVT) setDirection('')
  })
  if (!supportsVT && arrived) setTimeout(() => setDirection(''), 1000)

  // Переход по ссылке: запоминаем направление (на случай браузера без pageswap), а без View
  // Transitions сначала проигрываем уход и только потом переходим.
  document.addEventListener('click', (event) => {
    const link = event.target.closest && event.target.closest('a[href]')
    if (!link || event.defaultPrevented) return
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    if ((link.target && link.target !== '_self') || link.hasAttribute('download')) return
    const url = new URL(link.href, location.href)
    if (url.origin !== location.origin) return
    const dir = directionOf(location.href, url.href)
    const samePage = url.pathname === location.pathname && url.search === location.search

    // Экран внутри app.html: перелистываем без перезагрузки.
    if (samePage) {
      if (url.hash && url.hash !== location.hash && document.getElementById('view')) {
        event.preventDefault()
        switchView(url.hash, dir)
      }
      return
    }

    rememberDirection(dir, url.href)
    if (supportsVT || reduced()) return
    event.preventDefault()
    setDirection(dir)
    html.classList.add('mx-leave')
    setTimeout(() => { location.href = url.href }, 240)
  })
  // Возврат кнопкой «назад» из кэша браузера не должен оставлять страницу погасшей.
  window.addEventListener('pageshow', () => html.classList.remove('mx-leave'))

  /* ---------- Экраны внутри app.html (Меню, Горячее, Сроки, Демо) ---------- */

  let viewTransitionRunning = false

  // app.js перерисовывает экран по hashchange; ждём, пока он отработает, и только тогда
  // браузер снимает новое состояние — старое к этому моменту уже сфотографировано.
  function switchView(hash, dir) {
    if (!sameDocVT || reduced()) {
      location.hash = hash
      return
    }
    setDirection(dir)
    viewTransitionRunning = true
    const transition = document.startViewTransition(() => new Promise((resolve) => {
      window.addEventListener('hashchange', () => setTimeout(resolve, 0), { once: true })
      location.hash = hash
    }))
    transition.finished.finally(() => {
      viewTransitionRunning = false
      setDirection('')
    })
  }

  // Смена экрана без View Transitions (кнопки «назад/вперёд» браузера, переход из скрипта).
  window.addEventListener('hashchange', (event) => {
    if (viewTransitionRunning || reduced()) return
    const view = document.getElementById('view')
    if (!view) return
    setDirection(directionOf(event.oldURL, event.newURL))
    view.classList.remove('mx-view')
    void view.offsetWidth
    view.classList.add('mx-view')
    setTimeout(() => setDirection(''), 1000)
  })
})()
