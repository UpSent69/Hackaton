/* Обмен данными с сервером.
 *
 * Весь интерфейс получает и изменяет данные только через API.get / post / del —
 * это обычные fetch-запросы к бэкенду. Формат запросов и ответов описан в API.md
 * и contract.go в папке backend-contract: бэкенд реализует именно его.
 *
 * Клиент всегда обращается к реальному backend. Проверки сессии и прав выполняет сервер;
 * здесь токен лишь прикладывается к API-запросам.
 */
const API = (() => {
  const CONFIG = {
    // Все маршруты бэкенда лежат под этим префиксом. Локально и в Docker nginx
    // проксирует его на Go-сервер, поэтому фронт и API живут на одном адресе.
    baseUrl: window.STOPLIST_API || '/api',
  }

  const TOKEN_KEY = 'stoplist.token'
  const USER_KEY = 'stoplist.user'

  class ApiError extends Error {
    constructor(status, message, data) {
      super(message || 'Ошибка запроса (' + status + ')')
      this.status = status
      // Тело ответа с подробностями, например список нехваток при отказе в заказе.
      this.data = data || null
    }
  }

  /* ---------- Сессия ---------- */

  const getToken = () => localStorage.getItem(TOKEN_KEY)

  // Cached user data is display-only. The backend decides whether its token/session is valid.
  function getUser() {
    try {
      return JSON.parse(localStorage.getItem(USER_KEY)) || null
    } catch {
      return null
    }
  }

  function setSession(session) {
    localStorage.setItem(TOKEN_KEY, session.token)
    localStorage.setItem(USER_KEY, JSON.stringify(session.user))
  }

  function clearSession() {
    localStorage.removeItem(TOKEN_KEY)
    localStorage.removeItem(USER_KEY)
  }

  /* ---------- Время и ревизия данных ---------- */

  // Часы сервера: в демо-режиме время на сервере может идти быстрее реального,
  // поэтому подписи вида «через 3 ч» считаются от серверного «сейчас».
  let clock = null // { base, anchor, speed }: серверное время base соответствует местному anchor
  let lastRev = 0

  function syncClock(state) {
    if (!state || typeof state.now !== 'string' || typeof state.speed !== 'number') return
    clock = { base: new Date(state.now).getTime(), anchor: Date.now(), speed: state.speed }
  }

  const serverNow = () => (clock ? clock.base + (Date.now() - clock.anchor) * clock.speed : Date.now())

  /* ---------- Запрос к серверу ---------- */

  async function remote(method, path, body) {
    const headers = { Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    const token = getToken()
    if (token) headers.Authorization = 'Bearer ' + token

    let response
    try {
      response = await fetch(CONFIG.baseUrl + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      })
    } catch {
      throw new ApiError(0, 'Нет связи с сервером')
    }

    // Сервер сообщает номер ревизии данных в заголовке каждого ответа.
    const rev = Number(response.headers.get('X-Data-Rev'))
    if (rev) lastRev = rev

    if (response.status === 204) return null
    const data = await response.json().catch(() => null)
    if (!response.ok) {
      const down = response.status >= 502 && response.status <= 504
      throw new ApiError(response.status, down ? 'Сервер недоступен' : data && data.message, data)
    }
    // Ответы с временем сервера (опрос состояния, демо-режим) заодно сверяют часы.
    syncClock(data)
    return data
  }

  async function upload(path, file) {
    const headers = { Accept: 'application/json' }
    const token = getToken()
    if (token) headers.Authorization = 'Bearer ' + token
    const form = new FormData()
    form.append('file', file)

    let response
    try {
      response = await fetch(CONFIG.baseUrl + path, { method: 'POST', headers, body: form })
    } catch {
      throw new ApiError(0, 'Нет связи с сервером')
    }

    const rev = Number(response.headers.get('X-Data-Rev'))
    if (rev) lastRev = rev
    const data = await response.json().catch(() => null)
    if (!response.ok) {
      const down = response.status >= 502 && response.status <= 504
      const tooBig = response.status === 413 && !(data && data.message)
      throw new ApiError(response.status, down ? 'Сервер недоступен' : tooBig ? 'Файл слишком большой: максимум 12 МБ' : data && data.message, data)
    }
    syncClock(data)
    return data
  }

  // Распознавание документа идёт в очереди на сервере: ответ 202 с номером задачи.
  // awaitJob переспрашивает /jobs/{id}, пока результат не готов, и возвращает его.
  async function awaitJob(result) {
    const started = Date.now()
    while (result && result.status === 'pending' && result.jobId) {
      if (Date.now() - started > 5 * 60 * 1000) throw new ApiError(504, 'Распознавание заняло больше 5 минут. Попробуйте ещё раз.')
      await new Promise((resolve) => setTimeout(resolve, 2000))
      result = await remote('GET', '/jobs/' + encodeURIComponent(result.jobId))
    }
    return result
  }

  /* ---------- Публичный интерфейс ---------- */

  return {
    ApiError,
    get: (path) => remote('GET', path),
    post: (path, body) => remote('POST', path, body),
    upload: (path, file) => upload(path, file),
    awaitJob,
    del: (path) => remote('DELETE', path),
    getUser,
    setSession,
    updateUser: (user) => localStorage.setItem(USER_KEY, JSON.stringify(user)),
    clearSession,
    // Текущее время сервера — им пользуются все подписи «через N ч».
    now: () => serverNow(),
    // Ревизия данных после последнего собственного запроса.
    rev: () => lastRev,
  }
})()
