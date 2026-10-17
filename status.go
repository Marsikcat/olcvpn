package main

// statusText is what a person reads about the connection — the headline and
// the hint under it. The window draws its own from /api/state (view() in
// web/index.html); this copy is for places without a page, such as the
// Android notification, and must say the same thing.
func statusText(ph phase, detail string, peer, whole bool) (title, hint string) {
	switch ph {
	case phaseStopped:
		return "Отключено", "Нажмите, чтобы подключиться"
	case phaseStarting:
		if detail == "" {
			detail = "Ищем сервер в комнате"
		}
		return "Подключение…", detail
	case phaseCaptcha:
		return "Проверка VK", "Нажмите «Я не робот» на странице VK"
	case phaseError:
		if detail == "" {
			detail = "Попробуйте ещё раз"
		}
		return "Не удалось", detail
	case phaseWaiting:
		if peer {
			return "Восстановление…", "Связь рвётся, идёт переподключение"
		}
		return "Сервер не отвечает", "Попробуйте другой сервер в списке"
	}
	if whole {
		return "Защищено", "Весь трафик идёт через туннель"
	}
	return "Прокси включён", "Работает только для приложений с этим прокси"
}

// running reports whether a phase means a session is up or coming up.
func (p phase) running() bool {
	return p != phaseStopped && p != phaseError
}
