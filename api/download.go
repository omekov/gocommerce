package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/jinzhu/gorm"
	gcontext "github.com/netlify/gocommerce/context"
	"github.com/netlify/gocommerce/models"
)

const maxIPsPerDay = 50

// DownloadURL returns a signed URL to download a purchased asset.
func (a *API) DownloadURL(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	db := a.DB(r)
	downloadID := chi.URLParam(r, "download_id")
	logEntrySetField(r, "download_id", downloadID)
	claims := gcontext.GetClaims(ctx)
	assets := gcontext.GetAssetStore(ctx)

	download := &models.Download{}
	if result := db.Where("id = ?", downloadID).First(download); result.Error != nil {
		if result.RecordNotFound() {
			return notFoundError("Download not found")
		}
		return internalServerError("Error during database query").WithInternalError(result.Error)
	}

	order := &models.Order{}
	if result := db.Where("id = ?", download.OrderID).First(order); result.Error != nil {
		if result.RecordNotFound() {
			return notFoundError("Download order not found")
		}
		return internalServerError("Error during database query").WithInternalError(result.Error)
	}

	if !hasOrderAccess(ctx, order) {
		return unauthorizedError("Not Authorized to access this download")
	}

	if order.PaymentState != models.PaidState {
		return unauthorizedError("This download has not been paid yet")
	}

	rows, err := db.Model(&models.Event{}).
		Select("count(distinct(ip))").
		Where("order_id = ? and created_at > ? and changes = 'download'", order.ID, time.Now().Add(-24*time.Hour)).
		Rows()
	if err != nil {
		return internalServerError("Error signing download").WithInternalError(err)
	}
	var count uint64
	for rows.Next() {
		err = rows.Scan(&count)
		if err != nil {
			return internalServerError("Error signing download").WithInternalError(err)
		}
	}
	if count > maxIPsPerDay {
		return unauthorizedError("This download has been accessed from too many IPs within the last day")
	}

	if err := download.SignURL(assets); err != nil {
		return internalServerError("Error signing download").WithInternalError(err)
	}

	tx := db.Begin()
	tx.Model(download).Updates(map[string]interface{}{"download_count": gorm.Expr("download_count + 1")})
	var subject string
	if claims != nil {
		subject = claims.Subject
	}
	models.LogEvent(tx, r.RemoteAddr, subject, order.ID, models.EventUpdated, []string{"download"})
	tx.Commit()

	return sendJSON(w, http.StatusOK, download)
}

// DownloadList lists all purchased downloads for an order or a user.
func (a *API) DownloadList(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	db := a.DB(r)
	orderID := gcontext.GetOrderID(ctx)
	log := getLogEntry(r)

	order := &models.Order{}
	if orderID != "" {
		if result := db.Where("id = ?", orderID).First(order); result.Error != nil {
			if result.RecordNotFound() {
				return notFoundError("Download order not found")
			}
			return internalServerError("Error during database query").WithInternalError(result.Error)
		}
	} else {
		order = nil
	}

	if order != nil {
		if !hasOrderAccess(ctx, order) {
			return unauthorizedError("You don't have permission to access this order")
		}

		if order.PaymentState != models.PaidState {
			return unauthorizedError("This order has not been completed yet")
		}
	}

	orderTable := db.NewScope(models.Order{}).QuotedTableName()
	downloadsTable := db.NewScope(models.Download{}).QuotedTableName()

	query := db.Joins("join " + orderTable + " ON " + downloadsTable + ".order_id = " + orderTable + ".id and " + orderTable + ".payment_state = 'paid'")
	if order != nil {
		query = query.Where(orderTable+".id = ?", order.ID)
	} else {
		claims := gcontext.GetClaims(ctx)
		query = query.Where(orderTable+".user_id = ?", claims.Subject)
	}

	offset, limit, err := paginate(w, r, query.Model(&models.Download{}))
	if err != nil {
		return badRequestError("Bad Pagination Parameters: %v", err)
	}

	var downloads []models.Download
	if result := query.Offset(offset).Limit(limit).Find(&downloads); result.Error != nil {
		return internalServerError("Error during database query").WithInternalError(err)
	}

	log.WithField("download_count", len(downloads)).Debugf("Successfully retrieved %d downloads", len(downloads))
	return sendJSON(w, http.StatusOK, downloads)
}

// DownloadRefresh makes sure downloads are up to date
func (a *API) DownloadRefresh(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	orderID := gcontext.GetOrderID(ctx)
	config := gcontext.GetConfig(ctx)
	log := getLogEntry(r)

	order := &models.Order{}
	if orderID == "" {
		return badRequestError("Order id missing")
	}

	query := a.db.Where("id = ?", orderID).
		Preload("LineItems").
		Preload("Downloads")
	if result := query.First(order); result.Error != nil {
		if result.RecordNotFound() {
			return notFoundError("Download order not found")
		}
		return internalServerError("Error during database query").WithInternalError(result.Error)
	}

	if !hasOrderAccess(ctx, order) {
		return unauthorizedError("You don't have permission to access this order")
	}

	if order.PaymentState != models.PaidState {
		return unauthorizedError("This order has not been completed yet")
	}

	if err := order.UpdateDownloads(config, log); err != nil {
		return internalServerError("Error during updating downloads").WithInternalError(err)
	}

	if result := a.db.Save(order); result.Error != nil {
		return internalServerError("Error during saving order").WithInternalError(result.Error)
	}

	return sendJSON(w, http.StatusOK, map[string]string{})
}
