/* Общий чат сети: плавающая кнопка в правом нижнем углу на всех страницах сайта.
 *
 * Пишут сотрудники всех трёх ресторанов, владелец и поставщик. Рядом с сообщением — первая
 * буква должности автора, над сообщением — его ресторан («Вся сеть» у владельца и поставщика).
 * Пока панель открыта, чат обновляется каждые 3 секунды; закрытая панель раз в 10 секунд
 * проверяет только новые сообщения — для счётчика на кнопке.
 */
;(() => {
  if (typeof API === 'undefined' || !API.getUser() || /login\.html$/.test(location.pathname)) return

  const ROLE = {
    owner: ['В', 'Владелец'],
    director: ['Д', 'Директор'],
    manager: ['М', 'Менеджер'],
    waiter: ['О', 'Официант'],
    supplier: ['П', 'Поставщик'],
  }
  const me = API.getUser()
  const SEEN_KEY = 'stoplist.chat.seen.' + me.id

  const esc = (value) =>
    String(value ?? '').replace(/[&<>"']/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[ch])
  const getSeen = () => {
    try {
      return Number(localStorage.getItem(SEEN_KEY)) || 0
    } catch {
      return 0
    }
  }
  const setSeen = (id) => {
    try {
      localStorage.setItem(SEEN_KEY, String(id))
    } catch {}
  }
  const time = (value) => {
    const date = new Date(value)
    const today = new Date().toDateString() === date.toDateString()
    const hm = date.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' })
    return today ? hm : date.toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit' }) + ', ' + hm
  }

  const style = document.createElement('style')
  style.textContent = `
    .chat-fab { position: fixed; right: 24px; bottom: 24px; z-index: 300; width: 56px; height: 56px; border-radius: 50%;
      border: 1px solid rgba(255,255,255,.18); background: #f2f2f2; color: #0a0a0b; cursor: pointer;
      display: flex; align-items: center; justify-content: center; box-shadow: 0 10px 30px rgba(0,0,0,.45);
      transition: transform .2s ease, background .2s ease; }
    .chat-fab:hover { transform: translateY(-2px); background: #fff; }
    .chat-fab svg { width: 24px; height: 24px; }
    .chat-badge { position: absolute; top: -4px; right: -4px; min-width: 20px; height: 20px; padding: 0 6px; border-radius: 10px;
      background: #ff5f5f; color: #fff; font: 600 11px/20px Inter, system-ui, sans-serif; text-align: center; }
    .chat-badge[hidden] { display: none; }
    .chat-panel { position: fixed; right: 24px; bottom: 92px; z-index: 300; width: min(380px, calc(100vw - 32px));
      height: min(560px, calc(100vh - 120px)); display: flex; flex-direction: column; border-radius: 16px;
      background: #111114; border: 1px solid rgba(255,255,255,.12); box-shadow: 0 24px 60px rgba(0,0,0,.6);
      color: #eee; font-family: Inter, system-ui, sans-serif; overflow: hidden;
      opacity: 0; transform: translateY(12px) scale(.98); pointer-events: none; transition: opacity .2s ease, transform .2s ease; }
    .chat-panel.open { opacity: 1; transform: none; pointer-events: auto; }
    .chat-head { display: flex; align-items: center; justify-content: space-between; padding: 14px 16px; border-bottom: 1px solid rgba(255,255,255,.08); }
    .chat-head b { font-size: 15px; font-weight: 600; letter-spacing: -.01em; }
    .chat-head span { display: block; font-size: 11.5px; color: #8a8a95; margin-top: 2px; }
    .chat-close { background: none; border: 0; color: #8a8a95; font-size: 22px; line-height: 1; cursor: pointer; padding: 4px 6px; }
    .chat-close:hover { color: #fff; }
    .chat-list { flex: 1; overflow-y: auto; padding: 12px 14px; display: flex; flex-direction: column; gap: 12px; }
    .chat-empty { margin: auto; color: #8a8a95; font-size: 13px; text-align: center; }
    .chat-msg { display: flex; flex-direction: column; align-items: flex-start; max-width: 100%; }
    .chat-msg.mine { align-items: flex-end; }
    .chat-rest { font-size: 10.5px; font-weight: 600; letter-spacing: .06em; text-transform: uppercase; color: #8a8a95; margin: 0 0 4px 38px; }
    .chat-msg.mine .chat-rest { margin: 0 38px 4px 0; }
    .chat-row { display: flex; align-items: flex-end; gap: 8px; max-width: 100%; }
    .chat-msg.mine .chat-row { flex-direction: row-reverse; }
    .chat-role { flex: none; width: 30px; height: 30px; border-radius: 50%; display: flex; align-items: center; justify-content: center;
      background: rgba(255,255,255,.1); border: 1px solid rgba(255,255,255,.16); font-size: 13px; font-weight: 600; color: #fff; cursor: default; }
    .chat-bubble { min-width: 0; max-width: 270px; padding: 8px 12px 9px; border-radius: 14px 14px 14px 4px; background: rgba(255,255,255,.07); }
    .chat-msg.mine .chat-bubble { border-radius: 14px 14px 4px 14px; background: #f2f2f2; color: #0a0a0b; }
    .chat-meta { display: flex; align-items: baseline; gap: 8px; font-size: 11.5px; margin-bottom: 3px; }
    .chat-meta b { font-weight: 600; }
    .chat-meta time { color: #8a8a95; }
    .chat-msg.mine .chat-meta time { color: #6b6b75; }
    .chat-del { margin-left: auto; background: none; border: 0; color: inherit; opacity: .45; cursor: pointer; font-size: 14px; line-height: 1; padding: 0 0 0 6px; }
    .chat-del:hover { opacity: 1; }
    .chat-text { font-size: 13.5px; line-height: 1.45; white-space: pre-wrap; overflow-wrap: anywhere; }
    .chat-form { display: flex; gap: 8px; padding: 12px; border-top: 1px solid rgba(255,255,255,.08); }
    .chat-form textarea { flex: 1; resize: none; height: 42px; max-height: 120px; padding: 11px 12px; border-radius: 10px;
      border: 1px solid rgba(255,255,255,.12); background: rgba(255,255,255,.04); color: #eee; font: 13.5px/1.4 Inter, system-ui, sans-serif; outline: none; }
    .chat-form textarea:focus { border-color: rgba(255,255,255,.35); }
    .chat-form button { flex: none; width: 42px; height: 42px; border-radius: 10px; border: 0; background: #f2f2f2; color: #0a0a0b; cursor: pointer;
      display: flex; align-items: center; justify-content: center; }
    .chat-form button:disabled { opacity: .5; cursor: default; }
    .chat-error { padding: 0 12px 10px; font-size: 12px; color: #ff7a7a; }
    .chat-error:empty { display: none; }
    /* Всплывающие уведомления — над кнопкой чата */
    .toasts { bottom: 96px !important; }
    @media (max-width: 560px) {
      .chat-fab { right: 16px; bottom: 16px; }
      .chat-panel { right: 16px; bottom: 84px; }
    }
  `
  document.head.append(style)

  const fab = document.createElement('button')
  fab.type = 'button'
  fab.className = 'chat-fab'
  fab.setAttribute('aria-label', 'Чат сети')
  fab.title = 'Чат сети'
  fab.innerHTML =
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>' +
    '<span class="chat-badge" hidden></span>'
  const panel = document.createElement('section')
  panel.className = 'chat-panel'
  panel.setAttribute('aria-label', 'Чат сети')
  panel.innerHTML = `
    <div class="chat-head"><div><b>Чат сети</b><span>Сотрудники всех ресторанов</span></div>
      <button type="button" class="chat-close" aria-label="Закрыть чат">×</button></div>
    <div class="chat-list" role="log" aria-live="polite"><div class="chat-empty">Загрузка…</div></div>
    <div class="chat-error" role="status"></div>
    <form class="chat-form">
      <textarea rows="1" maxlength="1000" placeholder="Сообщение…" aria-label="Сообщение"></textarea>
      <button type="submit" aria-label="Отправить"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 2 11 13"/><path d="M22 2 15 22l-4-9-9-4 20-7z"/></svg></button>
    </form>`
  document.body.append(fab, panel)

  const badge = fab.querySelector('.chat-badge')
  const list = panel.querySelector('.chat-list')
  const errorBox = panel.querySelector('.chat-error')
  const form = panel.querySelector('.chat-form')
  const input = form.querySelector('textarea')
  let open = false
  let stopped = false
  let timer = null
  let lastRendered = ''

  function render(messages) {
    const html = messages.length
      ? messages
          .map((m) => {
            const [letter, title] = ROLE[m.role] || ['?', m.role]
            return `<div class="chat-msg${m.mine ? ' mine' : ''}" data-id="${esc(m.id)}">
              <div class="chat-rest">${esc(m.restaurant)}</div>
              <div class="chat-row">
                <span class="chat-role" title="${esc(title)}">${letter}</span>
                <div class="chat-bubble">
                  <div class="chat-meta"><b>${esc(m.author)}</b><time>${time(m.at)}</time>${m.canDelete ? '<button type="button" class="chat-del" title="Удалить сообщение" aria-label="Удалить">×</button>' : ''}</div>
                  <div class="chat-text">${esc(m.text)}</div>
                </div>
              </div>
            </div>`
          })
          .join('')
      : '<div class="chat-empty">Сообщений пока нет — напишите первым.</div>'
    if (html === lastRendered) return
    const atBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 40
    list.innerHTML = html
    lastRendered = html
    if (atBottom || !list.dataset.scrolled) {
      list.scrollTop = list.scrollHeight
      list.dataset.scrolled = '1'
    }
  }

  async function refresh() {
    if (stopped) return
    try {
      if (open) {
        const messages = await API.get('/chat')
        render(messages)
        if (messages.length) setSeen(Number(messages[messages.length - 1].id))
        badge.hidden = true
      } else {
        const fresh = await API.get('/chat?after=' + getSeen())
        const unread = fresh.filter((m) => !m.mine).length
        badge.hidden = unread === 0
        badge.textContent = unread > 99 ? '99+' : String(unread)
      }
    } catch (error) {
      // Сессия кончилась — чат замолкает; сетевые сбои просто повторятся на следующем опросе.
      if (error.status === 401) stopped = true
    }
  }

  function schedule() {
    clearInterval(timer)
    timer = setInterval(refresh, open ? 3000 : 10000)
  }

  function toggle(state) {
    open = state
    panel.classList.toggle('open', open)
    fab.setAttribute('aria-expanded', String(open))
    if (open) {
      refresh()
      setTimeout(() => input.focus(), 50)
    }
    schedule()
  }

  fab.addEventListener('click', () => toggle(!open))
  panel.querySelector('.chat-close').addEventListener('click', () => toggle(false))
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && open) toggle(false)
  })

  input.addEventListener('keydown', (event) => {
    // Enter — отправить, Shift+Enter — новая строка.
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      form.requestSubmit()
    }
  })
  form.addEventListener('submit', async (event) => {
    event.preventDefault()
    const text = input.value.trim()
    if (!text) return
    const button = form.querySelector('button')
    button.disabled = true
    errorBox.textContent = ''
    try {
      await API.post('/chat', { text })
      input.value = ''
      list.dataset.scrolled = ''
      await refresh()
    } catch (error) {
      errorBox.textContent = error.message || 'Не удалось отправить сообщение'
    } finally {
      button.disabled = false
      input.focus()
    }
  })

  list.addEventListener('click', async (event) => {
    const button = event.target.closest('.chat-del')
    if (!button || !window.confirm('Удалить сообщение?')) return
    const id = button.closest('.chat-msg').dataset.id
    try {
      await API.del('/chat/' + encodeURIComponent(id))
      await refresh()
    } catch (error) {
      errorBox.textContent = error.message || 'Не удалось удалить сообщение'
    }
  })

  refresh()
  schedule()
})()
