package handler

import (
	"context"

	notificationv1 "EXBanka/gen/proto/notification/v1"
	"EXBanka/internal/config"
	infrasvc "EXBanka/internal/service"
)

type NotificationHandler struct {
	notificationv1.UnimplementedNotificationServiceServer
	cfg      *config.Config
	notifSvc *infrasvc.NotificationService
}

func NewNotificationHandler(cfg *config.Config, notifSvc *infrasvc.NotificationService) *NotificationHandler {
	return &NotificationHandler{cfg: cfg, notifSvc: notifSvc}
}

func (h *NotificationHandler) SendActivationEmail(ctx context.Context, req *notificationv1.SendActivationEmailRequest) (*notificationv1.SendEmailResponse, error) {
	err := h.notifSvc.SendActivationEmail(req.ToEmail, req.ToName, req.ActivationToken)
	if err != nil {
		return &notificationv1.SendEmailResponse{Success: false, Message: err.Error()}, nil
	}
	return &notificationv1.SendEmailResponse{Success: true, Message: "Activation email sent"}, nil
}

func (h *NotificationHandler) SendResetPasswordEmail(ctx context.Context, req *notificationv1.SendResetPasswordEmailRequest) (*notificationv1.SendEmailResponse, error) {
	err := h.notifSvc.SendResetPasswordEmail(req.ToEmail, req.ToName, req.ResetToken)
	if err != nil {
		return &notificationv1.SendEmailResponse{Success: false, Message: err.Error()}, nil
	}
	return &notificationv1.SendEmailResponse{Success: true, Message: "Reset email sent"}, nil
}

func (h *NotificationHandler) SendConfirmationEmail(ctx context.Context, req *notificationv1.SendConfirmationEmailRequest) (*notificationv1.SendEmailResponse, error) {
	err := h.notifSvc.SendConfirmationEmail(req.ToEmail, req.ToName)
	if err != nil {
		return &notificationv1.SendEmailResponse{Success: false, Message: err.Error()}, nil
	}
	return &notificationv1.SendEmailResponse{Success: true, Message: "Confirmation email sent"}, nil
}
