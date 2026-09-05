# Neva Concert Hall Corporate Approvals

Go/Gin/HTMX-приложение с PostgreSQL и закрытым входом для Timeweb Cloud App Platform.

## Локальный запуск

```bash
go mod download
go run main.go
```

Откройте `http://localhost:8080`. Проверка состояния доступна по адресу `/health`.

Для запуска нужны переменные `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`,
`PGSSLMODE`, `SESSION_SECRET`, `INITIAL_ADMIN_EMAIL`, `INITIAL_ADMIN_PASSWORD` и
опционально `INITIAL_ADMIN_NAME`.

При первом запуске приложение создаст таблицы `users` и `sessions`, а затем первого
администратора. После успешного входа `INITIAL_ADMIN_PASSWORD` следует удалить из
переменных App Platform: хэш пароля уже сохранен в PostgreSQL.

## Настройки Timeweb

- Тип: Backend → Gin
- Команда запуска: `go run main.go`
- Путь проверки состояния: `/health`
- Путь до директории проекта: пустой, если эти файлы находятся в корне репозитория
