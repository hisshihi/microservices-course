package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-faster/errors"
	orderv1 "github.com/hisshihi/microservices-course/pkg/openapi/order/v1"
)

const (
	httpAddr          = "8080"
	readHeaderTimeout = 5 * time.Second
	serverTimeout     = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

type Status string

const (
	PendingPayment Status = "pending_payment"
	Paid           Status = "paid"
	Cancelled      Status = "cancelled"
)

func (s Status) String() string {
	return string(s)
}

type PaymentMethod uint8

const (
	Unknown PaymentMethod = iota
	Card
	SBP
	CreditCard
	InvestorMoney
)

func (s PaymentMethod) String() string {
	return string(s)
}

type Order struct {
	UserUUID        string         `json:"user_uuid"`
	OrderUUID       string         `json:"order_uuid"`
	PartUUIDs       []string       `json:"part_uuids"`
	TotalPrice      float64        `json:"total_price"`
	TransactionUUID *string        `json:"transaction_uuid"` // Только если оплачено
	PaymentMethod   *PaymentMethod `json:"payment_method"`
	Status          Status         `json:"status"`
}

type OrderStorage struct {
	mu    sync.Mutex
	order map[string]*Order
}

func NewOrderStorage() *OrderStorage {
	return &OrderStorage{
		order: make(map[string]*Order),
	}
}

func (s *OrderStorage) Save(userUUID string, partUUIDs []string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.order[userUUID]
	if ok {
		order.PartUUIDs = append(order.PartUUIDs, partUUIDs...)
		order.Status = PendingPayment
		return *order, nil
	}
	order = &Order{
		UserUUID:  userUUID,
		OrderUUID: uuid.NewV7().String(),
		PartUUIDs: partUUIDs,
		Status:    PendingPayment,
	}
	s.order[userUUID] = order
	return *order, nil
}

type OrderHandler struct {
	orderStorage *OrderStorage
}

func NewOrderHandler(orderStorage *OrderStorage) *OrderHandler {
	return &OrderHandler{orderStorage: orderStorage}
}

func (h *OrderHandler) CreateOrder(ctx context.Context, req *orderv1.CreateOrderRequest) (orderv1.CreateOrderRes, error) {
	parts := make([]string, 0, len(req.GetPartUuids()))
	for _, part := range req.GetPartUuids() {
		parts = append(parts, part.String())
	}
	save, err := h.orderStorage.Save(req.UserUUID.String(), parts)
	if err != nil {
		log.Printf("Ошибка при сохранении заказа: %v\n", err)
		return nil, h.NewError(ctx, err)
	}

	return &orderv1.CreateOrderResponse{OrderUUID: save.OrderUUID, TotalPrice: float32(save.TotalPrice)}, nil
}

func (h *OrderHandler) NewError(_ context.Context, err error) *orderv1.GenericErrorStatusCode {
	return &orderv1.GenericErrorStatusCode{
		StatusCode: http.StatusInternalServerError,
		Response: orderv1.GenericError{
			Code:    orderv1.NewOptString("internal_error"),
			Message: orderv1.NewOptString(fmt.Sprintf("Ошибка при выполнении запроса: %v", err)),
		},
	}
}

func main() {
	storage := NewOrderStorage()

	handler := NewOrderHandler(storage)

	orderServer, err := orderv1.NewServer(handler)
	if err != nil {
		log.Fatalf("ошибка при создании сервера openAPI: %v", err)
	}

	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(serverTimeout))

	r.Mount("/", orderServer)

	server := &http.Server{
		Addr:              net.JoinHostPort("localhost", httpAddr),
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	go func() {
		log.Printf("запуск сервера на %s", server.Addr)
		if srvErr := server.ListenAndServe(); srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
			log.Printf("ошибка запуска сервера: %v", srvErr)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Println("Завершение работы")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err = server.Shutdown(ctx)
	if err != nil {
		log.Printf("ошибка при завершении работы сервера: %v", err)
	}
	log.Println("Сервер успешно остановлен")
}
