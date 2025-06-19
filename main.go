package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Client  string `json:"client"`
	Message string `json:"message"`
}

type ClientStats struct {
	Total    int       `json:"total"`
	LastMsg  string    `json:"last_msg"`
	LastTime time.Time `json:"last_time"`
	Speed    float64   `json:"speed"`
}

var (
	stats     = make(map[string]*ClientStats)
	statsLock sync.RWMutex
	clients   = make(map[chan []byte]bool)
	semaphore = make(chan struct{}, 1000) // Ограничение одновременных запросов
)

func main() {
	http.HandleFunc("/message", messageHandler)
	http.HandleFunc("/dashboard", dashboardHandler)
	http.HandleFunc("/events", sseHandler)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	server := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Println("Сервер запущен на порту 8080")
	go broadcastLoop()
	log.Fatal(server.ListenAndServe())
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	// Загружаем шаблон
	tmpl, err := template.ParseFiles("templates/dashboard.html")
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Получаем текущую статистику (с блокировкой для чтения)
	statsLock.RLock()
	defer statsLock.RUnlock()

	// Подготавливаем данные для шаблона
	data := struct {
		Clients []string
		Stats   map[string]ClientStats
	}{
		Clients: make([]string, 0, len(stats)),
		Stats:   make(map[string]ClientStats, len(stats)),
	}

	for client, stat := range stats {
		data.Clients = append(data.Clients, client)
		data.Stats[client] = *stat
	}

	// Рендерим шаблон
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Template execution error: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	} else {
	}
}
func messageHandler(w http.ResponseWriter, r *http.Request) {
	// Получаем информацию о клиенте
	clientIP := strings.Split(r.RemoteAddr, ":")[0]
	startTime := time.Now()

	// Логируем начало обработки запроса
	log.Printf("▶️ Начало обработки запроса от %s", clientIP)
	defer func() {
		log.Printf("⏹️ Завершение обработки запроса от %s (длительность: %v)", clientIP, time.Since(startTime))
	}()

	// Проверяем перегрузку сервера
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	default:
		msg := "🚨 Сервер перегружен, запрос отклонен"
		log.Println(msg)
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}

	// Читаем тело запроса
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("❌ Ошибка чтения тела запроса от %s: %v", clientIP, err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Логируем сырое тело запроса
	log.Printf("📨 Получено сырое сообщение от %s:\n%s", clientIP, string(body))

	// Парсим JSON
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("❌ Ошибка парсинга JSON от %s: %v\nДанные: %s", clientIP, err, string(body))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Логируем успешный парсинг
	log.Printf("✅ Успешно распарсено сообщение от %s:\nКлиент: %s\nСообщение: %s",
		clientIP, msg.Client, msg.Message)

	// Обновляем статистику
	statsLock.Lock()
	defer statsLock.Unlock()

	stat, exists := stats[msg.Client]
	if !exists {
		stat = &ClientStats{}
		stats[msg.Client] = stat
		log.Printf("🆕 Новый клиент зарегистрирован: %s", msg.Client)
	}

	stat.Total++
	stat.LastMsg = msg.Message
	stat.LastTime = time.Now()

	// Отправляем подтверждение
	w.WriteHeader(http.StatusAccepted)
	log.Printf("📤 Ответ отправлен клиенту %s", msg.Client)
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	// Настройка заголовков SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*") // Важно для CORS

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Создаем канал для клиента
	ch := make(chan []byte, 10)
	clients[ch] = true
	defer func() {
		delete(clients, ch)
		close(ch)
	}()

	// Отправляем начальные данные
	statsLock.RLock()
	initialData, _ := json.Marshal(stats)
	statsLock.RUnlock()

	fmt.Fprintf(w, "data: %s\n\n", initialData)
	flusher.Flush()

	// Обрабатываем соединение
	for {
		select {
		case msg := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				log.Printf("Ошибка отправки SSE: %v", err)
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func broadcastLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		statsLock.RLock()
		snapshot := make(map[string]interface{}, len(stats))

		for client, stat := range stats {
			speed := float64(stat.Total) / time.Since(stat.LastTime).Seconds()
			stat.Speed = speed
			snapshot[client] = map[string]interface{}{
				"Total":    stat.Total,
				"LastMsg":  stat.LastMsg,
				"LastTime": stat.LastTime.Format(time.RFC3339),
				"Speed":    stat.Speed,
			}
		}

		data, err := json.Marshal(snapshot)
		statsLock.RUnlock()

		if err != nil {
			log.Printf("Ошибка маршалинга статистики: %v", err)
			continue
		}

		// Отправка всем подключенным клиентам
		for ch := range clients {
			select {
			case ch <- data:
			default:
				log.Println("Канал клиента переполнен, пропускаем")
			}
		}
	}
}
