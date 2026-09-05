# Neva Concert Hall Corporate Approvals

Минимальное Go/Gin/HTMX-приложение для первого деплоя в Timeweb Cloud App Platform.

## Локальный запуск

```bash
go mod download
go run main.go
```

Откройте `http://localhost:8080`. Проверка состояния доступна по адресу `/health`.

## Настройки Timeweb

- Тип: Backend → Gin
- Команда запуска: `go run main.go`
- Путь проверки состояния: `/health`
- Путь до директории проекта: пустой, если эти файлы находятся в корне репозитория
