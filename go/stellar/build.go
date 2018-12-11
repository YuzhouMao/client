package stellar

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/stellar1"
	"github.com/keybase/client/go/slotctx"
	"github.com/keybase/client/go/stellar/stellarcommon"
	"github.com/keybase/stellarnet"
)

func StartBuildPaymentLocal(mctx libkb.MetaContext) (res stellar1.BuildPaymentID, err error) {
	return getGlobal(mctx.G()).startBuildPayment(mctx)
}

func StopBuildPaymentLocal(mctx libkb.MetaContext, bid stellar1.BuildPaymentID) {
	getGlobal(mctx.G()).stopBuildPayment(mctx, bid)
}

func BuildPaymentLocal(mctx libkb.MetaContext, arg stellar1.BuildPaymentLocalArg) (res stellar1.BuildPaymentResLocal, err error) {
	tracer := mctx.G().CTimeTracer(mctx.Ctx(), "BuildPaymentLocal", true)
	defer tracer.Finish()

	var data *buildPaymentData
	var release func()
	if arg.Bid.IsNil() {
		// Compatibility for pre-bid gui and tests.
		mctx = mctx.WithCtx(
			getGlobal(mctx.G()).buildPaymentSlot.Use(mctx.Ctx(), arg.SessionID))
	} else {
		mctx, data, release, err = getGlobal(mctx.G()).acquireBuildPayment(mctx, arg.Bid, arg.SessionID)
		defer release()
		if err != nil {
			return res, err
		}

		// Mark the payment as not ready to send while the new values are validated.
		data.ReadyToReview = false
		data.ReadyToSend = false
		data.Frozen = nil
	}

	readyChecklist := struct {
		from       bool
		to         bool
		amount     bool
		secretNote bool
		publicMemo bool
	}{}
	log := func(format string, args ...interface{}) {
		mctx.CDebugf("bpl: "+format, args...)
	}

	bpc := getGlobal(mctx.G()).getBuildPaymentCache()
	if bpc == nil {
		return res, fmt.Errorf("missing build payment cache")
	}

	// -------------------- from --------------------

	tracer.Stage("from")
	fromInfo := struct {
		available bool
		from      stellar1.AccountID
	}{}
	if arg.FromPrimaryAccount != arg.From.IsNil() {
		// Exactly one of `from` and `fromPrimaryAccount` must be set.
		return res, fmt.Errorf("invalid build payment parameters")
	}
	fromPrimaryAccount := arg.FromPrimaryAccount
	if arg.FromPrimaryAccount {
		primaryAccountID, err := bpc.PrimaryAccount(mctx)
		if err != nil {
			log("PrimaryAccount -> err:%v", err)
			res.Banners = append(res.Banners, stellar1.SendBannerLocal{
				Level:   "error",
				Message: "Could not find primary account.",
			})
		} else {
			fromInfo.from = primaryAccountID
			fromInfo.available = true
		}
	} else {
		owns, fromPrimary, err := bpc.OwnsAccount(mctx, arg.From)
		if err != nil || !owns {
			log("OwnsAccount (from) -> owns:%v err:%v", owns, err)
			res.Banners = append(res.Banners, stellar1.SendBannerLocal{
				Level:   "error",
				Message: "Could not find source account.",
			})
		} else {
			fromInfo.from = arg.From
			fromInfo.available = true
			fromPrimaryAccount = fromPrimary
		}
	}
	if fromInfo.available {
		res.From = fromInfo.from
		readyChecklist.from = true
	}

	// -------------------- to --------------------

	tracer.Stage("to")
	skipRecipient := len(arg.To) == 0
	var minAmountXLM string
	if !skipRecipient && arg.ToIsAccountID {
		_, err := libkb.ParseStellarAccountID(arg.To)
		if err != nil {
			res.ToErrMsg = err.Error()
			skipRecipient = true
		} else {
			readyChecklist.to = true
		}
	}
	if !skipRecipient {
		recipient, err := bpc.LookupRecipient(mctx, stellarcommon.RecipientInput(arg.To))
		if err != nil {
			log("error with recipient field %v: %v", arg.To, err)
			res.ToErrMsg = "Recipient not found."
			skipRecipient = true
		} else {
			bannerThey := "they"
			bannerTheir := "their"
			if recipient.User != nil && !arg.ToIsAccountID {
				bannerThey = recipient.User.Username.String()
				bannerTheir = fmt.Sprintf("%s's", recipient.User.Username)
			}
			if recipient.AccountID == nil && !fromPrimaryAccount {
				// This would have been a relay from a non-primary account.
				// We cannot allow that.
				res.Banners = append(res.Banners, stellar1.SendBannerLocal{
					Level:   "error",
					Message: fmt.Sprintf("Because %v hasn’t set up their wallet yet, you can only send to them from your default account.", bannerThey),
				})
			} else {
				readyChecklist.to = true
				addMinBanner := func(them, amount string) {
					res.Banners = append(res.Banners, stellar1.SendBannerLocal{
						Level:   "info",
						Message: fmt.Sprintf("Because it's %s first transaction, you must send at least %s XLM.", them, amount),
					})
				}
				if recipient.AccountID == nil {
					// Sending a payment to a target with no account. (relay)
					minAmountXLM = "2.01"
					addMinBanner(bannerTheir, minAmountXLM)
				} else {
					isFunded, err := bpc.IsAccountFunded(mctx, stellar1.AccountID(recipient.AccountID.String()))
					if err != nil {
						log("error checking recipient funding status %v: %v", *recipient.AccountID, err)
					} else if !isFunded {
						// Sending to a non-funded stellar account.
						minAmountXLM = "1"
						owns, _, err := bpc.OwnsAccount(mctx, stellar1.AccountID(recipient.AccountID.String()))
						log("OwnsAccount (to) -> owns:%v err:%v", owns, err)
						if !owns || err != nil {
							// Likely sending to someone else's account.
							addMinBanner(bannerTheir, minAmountXLM)
						} else {
							// Sending to our own account.
							res.Banners = append(res.Banners, stellar1.SendBannerLocal{
								Level:   "info",
								Message: fmt.Sprintf("Because it's the first transaction on your receiving account, you must send at least %v.", minAmountXLM),
							})
						}
					}
				}
			}
		}
	}

	// -------------------- amount + asset --------------------

	tracer.Stage("amount + asset")
	bpaArg := buildPaymentAmountArg{
		Amount:   arg.Amount,
		Currency: arg.Currency,
		Asset:    arg.Asset,
	}
	if fromInfo.available {
		bpaArg.From = &fromInfo.from
	}
	amountX := buildPaymentAmountHelper(mctx, bpc, bpaArg)
	res.AmountErrMsg = amountX.amountErrMsg
	res.WorthDescription = amountX.worthDescription
	res.WorthInfo = amountX.worthInfo
	res.WorthCurrency = amountX.worthCurrency
	res.DisplayAmountXLM = amountX.displayAmountXLM
	res.DisplayAmountFiat = amountX.displayAmountFiat
	res.SendingIntentionXLM = amountX.sendingIntentionXLM

	if amountX.haveAmount {
		if !amountX.asset.IsNativeXLM() {
			return res, fmt.Errorf("sending non-XLM assets is not supported")
		}
		readyChecklist.amount = true

		if fromInfo.available {
			// Check that the sender has enough asset available.
			// Note: When adding support for sending non-XLM assets, check the asset instead of XLM here.
			availableToSendXLM, err := bpc.AvailableXLMToSend(mctx, fromInfo.from)
			availableToSendXLM = SubtractFeeSoft(mctx, availableToSendXLM)
			if err != nil {
				log("error getting available balance: %v", err)
			} else {
				cmp, err := stellarnet.CompareStellarAmounts(availableToSendXLM, amountX.amountOfAsset)
				switch {
				case err != nil:
					log("error comparing amounts (%v) (%v): %v", availableToSendXLM, amountX.amountOfAsset, err)
				case cmp == -1:
					log("Send amount is more than available to send %v > %v", amountX.amountOfAsset, availableToSendXLM)
					readyChecklist.amount = false // block sending
					res.AmountErrMsg = fmt.Sprintf("Your available to send is *%s XLM*.", availableToSendXLM)
					availableToSendXLMFmt, err := FormatAmount(
						availableToSendXLM, false, FmtTruncate)
					if err == nil {
						res.AmountErrMsg = fmt.Sprintf("Your available to send is *%s XLM*.", availableToSendXLMFmt)
					}
					if arg.Currency != nil && amountX.rate != nil {
						// If the user entered an amount in outside currency and an exchange
						// rate is available, attempt to show them available balance in that currency.
						availableToSendOutside, err := stellarnet.ConvertXLMToOutside(availableToSendXLM, amountX.rate.Rate)
						if err != nil {
							log("error converting available-to-send", err)
						} else {
							formattedATS, err := FormatCurrencyWithCodeSuffix(mctx.Ctx(), mctx.G(),
								availableToSendOutside, amountX.rate.Currency, FmtTruncate)
							if err != nil {
								log("error formatting available-to-send", err)
							} else {
								res.AmountErrMsg = fmt.Sprintf("Your available to send is *%s*.", formattedATS)
							}
						}
					}
				default:
					// Welcome back. How was your stay at the error handling hotel?
				}
			}
		}

		if minAmountXLM != "" {
			cmp, err := stellarnet.CompareStellarAmounts(amountX.amountOfAsset, minAmountXLM)
			switch {
			case err != nil:
				log("error comparing amounts", err)
			case cmp == -1:
				// amount is less than minAmountXLM
				readyChecklist.amount = false // block sending
				res.AmountErrMsg = fmt.Sprintf("You must send at least *%s XLM*", minAmountXLM)
			}
		}

		// Note: When adding support for sending non-XLM assets, check here that the recipient accepts the asset.
	}

	// helper so the GUI doesn't have to call FormatCurrency separately
	if arg.Currency != nil {
		res.WorthAmount = amountX.amountOfAsset
	}

	// -------------------- note + memo --------------------

	tracer.Stage("note + memo")
	if len(arg.SecretNote) <= 500 {
		readyChecklist.secretNote = true
	} else {
		res.SecretNoteErrMsg = "Note is too long."
	}

	if len(arg.PublicMemo) <= 28 {
		readyChecklist.publicMemo = true
	} else {
		res.PublicMemoErrMsg = "Memo is too long."
	}

	// -------------------- end --------------------

	if readyChecklist.from && readyChecklist.to && readyChecklist.amount && readyChecklist.secretNote && readyChecklist.publicMemo {
		res.ReadyToReview = true

		if data != nil {
			// Mark the payment as ready to review.
			data.ReadyToReview = true
			data.ReadyToSend = false
			data.Frozen = &frozenPayment{
				From:          fromInfo.from,
				To:            arg.To,
				ToIsAccountID: arg.ToIsAccountID,
				Amount:        amountX.amountOfAsset,
				Asset:         amountX.asset,
				SecretNote:    arg.SecretNote,
				PublicMemo:    arg.PublicMemo,
			}
		}
	}

	// Return the context's error.
	// If just `nil` were returned then in the event of a cancellation
	// resilient parts of this function could hide it, causing
	// a bogus return value.
	return res, mctx.Ctx().Err()
}

type reviewButtonState string

const reviewButtonSpinning = "spinning"
const reviewButtonEnabled = "enabled"
const reviewButtonDisabled = "disabled"

func ReviewPaymentLocal(mctx libkb.MetaContext, stellarUI stellar1.UiInterface, arg stellar1.ReviewPaymentLocalArg) (err error) {
	tracer := mctx.G().CTimeTracer(mctx.Ctx(), "ReviewPaymentLocal", true)
	defer tracer.Finish()

	if arg.Bid.IsNil() {
		return fmt.Errorf("missing payment ID")
	}

	mctx, data, release, err := getGlobal(mctx.G()).acquireBuildPayment(mctx, arg.Bid, arg.SessionID)
	defer release()
	if err != nil {
		return err
	}

	notify := func(seqno int, banners []stellar1.SendBannerLocal, nextButton reviewButtonState) chan struct{} {
		receivedCh := make(chan struct{}) // channel closed when the notification has been acked.
		mctx.CDebugf("sending UIPaymentReview bid:%v sessionID:%v seqno:%v nextButton:%v banners:%v",
			arg.Bid, arg.SessionID, seqno, nextButton, len(banners))
		go func() {
			err := stellarUI.PaymentReviewed(mctx.Ctx(), stellar1.PaymentReviewedArg{
				SessionID: arg.SessionID,
				Msg: stellar1.UIPaymentReviewed{
					Bid:        arg.Bid,
					Seqno:      seqno,
					Banners:    banners,
					NextButton: string(nextButton),
				},
			})
			if err != nil {
				mctx.CDebugf("error in response to UIPaymentReview: %v", err)
			}
			close(receivedCh)
		}()
		return receivedCh
	}

	if !data.ReadyToReview {
		// Caller goofed.
		notify(1, []stellar1.SendBannerLocal{{
			Level:   "error",
			Message: "This payment is not ready to review",
		}}, reviewButtonDisabled)
		return fmt.Errorf("this payment is not ready to review")
	}
	if data.Frozen == nil {
		// Should be impossible.
		return fmt.Errorf("this payment is missing values")
	}

	notify(1, nil, reviewButtonSpinning)

	if data.Frozen.ToIsAccountID {
		mctx.CDebugf("skipping identify for account ID recipient: %v", data.Frozen.To)
	} else {
		// In the future this method will identify the recipient and check tracking.
		// But for now, imagine the identify succeeded.
		mctx.CDebugf("skipping identify of recipient: %v", data.Frozen.To)
	}

	data.ReadyToSend = true

	if err := mctx.Ctx().Err(); err != nil {
		return err
	}
	receivedEnableCh := notify(2, nil, reviewButtonEnabled)

	// Stay open until this call gets canceled or until frontend
	// acks a notification that enables the button.
	select {
	case <-receivedEnableCh:
	case <-mctx.Ctx().Done():
	}
	return mctx.Ctx().Err()
}

func BuildRequestLocal(mctx libkb.MetaContext, arg stellar1.BuildRequestLocalArg) (res stellar1.BuildRequestResLocal, err error) {
	tracer := mctx.G().CTimeTracer(mctx.Ctx(), "BuildRequestLocal", true)
	defer tracer.Finish()

	mctx = mctx.WithCtx(
		getGlobal(mctx.G()).buildPaymentSlot.Use(
			mctx.Ctx(), arg.SessionID))
	if err := mctx.Ctx().Err(); err != nil {
		return res, err
	}

	readyChecklist := struct {
		to         bool
		amount     bool
		secretNote bool
	}{}
	log := func(format string, args ...interface{}) {
		mctx.CDebugf("brl: "+format, args...)
	}

	bpc := getGlobal(mctx.G()).getBuildPaymentCache()
	if bpc == nil {
		return res, fmt.Errorf("missing build payment cache")
	}

	// -------------------- to --------------------

	tracer.Stage("to")
	skipRecipient := len(arg.To) == 0
	if !skipRecipient {
		_, err := bpc.LookupRecipient(mctx, stellarcommon.RecipientInput(arg.To))
		if err != nil {
			log("error with recipient field %v: %v", arg.To, err)
			res.ToErrMsg = "Recipient not found."
			skipRecipient = true
		} else {
			readyChecklist.to = true
		}
	}

	// -------------------- amount + asset --------------------

	tracer.Stage("amount + asset")
	bpaArg := buildPaymentAmountArg{
		Amount:   arg.Amount,
		Currency: arg.Currency,
		Asset:    arg.Asset,
	}

	// For requests From is always the primary account.
	primaryAccountID, err := bpc.PrimaryAccount(mctx)
	if err != nil {
		log("PrimaryAccount -> err:%v", err)
		res.Banners = append(res.Banners, stellar1.SendBannerLocal{
			Level:   "error",
			Message: "Could not find primary account.",
		})
	} else {
		bpaArg.From = &primaryAccountID
	}

	amountX := buildPaymentAmountHelper(mctx, bpc, bpaArg)
	res.AmountErrMsg = amountX.amountErrMsg
	res.WorthDescription = amountX.worthDescription
	res.WorthInfo = amountX.worthInfo
	res.DisplayAmountXLM = amountX.displayAmountXLM
	res.DisplayAmountFiat = amountX.displayAmountFiat
	res.SendingIntentionXLM = amountX.sendingIntentionXLM
	readyChecklist.amount = amountX.haveAmount

	// -------------------- note --------------------

	tracer.Stage("note")
	if len(arg.SecretNote) <= 500 {
		readyChecklist.secretNote = true
	} else {
		res.SecretNoteErrMsg = "Note is too long."
	}

	// -------------------- end --------------------

	if readyChecklist.to && readyChecklist.amount && readyChecklist.secretNote {
		res.ReadyToRequest = true
	}
	// Return the context's error.
	// If just `nil` were returned then in the event of a cancellation
	// resilient parts of this function could hide it, causing
	// a bogus return value.
	return res, mctx.Ctx().Err()
}

type buildPaymentAmountArg struct {
	// See buildPaymentLocal in avdl from which these args are copied.
	Amount   string
	Currency *stellar1.OutsideCurrencyCode
	Asset    *stellar1.Asset
	From     *stellar1.AccountID
}

type buildPaymentAmountResult struct {
	haveAmount       bool // whether `amountOfAsset` and `asset` are valid
	amountOfAsset    string
	asset            stellar1.Asset
	amountErrMsg     string
	worthDescription string
	worthInfo        string
	worthCurrency    string
	// Rate may be nil if there was an error fetching it.
	rate                *stellar1.OutsideExchangeRate
	displayAmountXLM    string
	displayAmountFiat   string
	sendingIntentionXLM bool
}

var zeroOrNoAmountRE = regexp.MustCompile(`^0*\.?0*$`)

func buildPaymentAmountHelper(mctx libkb.MetaContext, bpc BuildPaymentCache, arg buildPaymentAmountArg) (res buildPaymentAmountResult) {
	log := func(format string, args ...interface{}) {
		mctx.CDebugf("bpl: "+format, args...)
	}
	res.asset = stellar1.AssetNative()
	switch {
	case arg.Currency != nil && arg.Asset == nil:
		// Amount is of outside currency.
		res.sendingIntentionXLM = false
		convertAmountOutside := "0"

		if zeroOrNoAmountRE.MatchString(arg.Amount) {
			// Zero or no amount given. Still convert for 0.
		} else {
			amount, err := stellarnet.ParseAmount(arg.Amount)
			if err != nil || amount.Sign() < 0 {
				// Invalid or negative amount.
				res.amountErrMsg = "Invalid amount."
				return res
			}
			if amount.Sign() > 0 {
				// Only save the amount if it's non-zero. So that =="0" later works.
				convertAmountOutside = arg.Amount
			}
		}
		xrate, err := bpc.GetOutsideExchangeRate(mctx, *arg.Currency)
		if err != nil {
			log("error getting exchange rate for %v: %v", arg.Currency, err)
			res.amountErrMsg = fmt.Sprintf("Could not get exchange rate for %v", arg.Currency.String())
			return res
		}
		res.rate = &xrate
		xlmAmount, err := stellarnet.ConvertOutsideToXLM(convertAmountOutside, xrate.Rate)
		if err != nil {
			log("error converting: %v", err)
			res.amountErrMsg = fmt.Sprintf("Could not convert to XLM")
			return res
		}
		res.amountOfAsset = xlmAmount
		xlmAmountFormatted, err := FormatAmountDescriptionXLM(xlmAmount)
		if err != nil {
			log("error formatting converted XLM amount: %v", err)
			res.amountErrMsg = fmt.Sprintf("Could not convert to XLM")
			return res
		}
		res.worthDescription = xlmAmountFormatted
		res.worthCurrency = string(*arg.Currency)
		if convertAmountOutside != "0" {
			// haveAmount gates whether the send button is enabled.
			// Only enable after `worthDescription` is set.
			// Don't allow the user to send if they haven't seen `worthDescription`,
			// since that's what they are really sending.
			res.haveAmount = true
		}
		res.worthInfo, err = buildPaymentWorthInfo(mctx, xrate)
		if err != nil {
			log("error making worth info: %v", err)
			res.worthInfo = ""
		}

		res.displayAmountXLM = xlmAmountFormatted
		res.displayAmountFiat, err = FormatCurrencyWithCodeSuffix(mctx.Ctx(), mctx.G(),
			convertAmountOutside, *arg.Currency, FmtRound)
		if err != nil {
			log("error converting for displayAmountFiat: %q / %q : %s", convertAmountOutside, arg.Currency, err)
			res.displayAmountFiat = ""
		}

		return res
	case arg.Currency == nil:
		res.sendingIntentionXLM = true
		if arg.Asset != nil {
			res.asset = *arg.Asset
		}
		// Amount is of asset.
		useAmount := "0"
		if zeroOrNoAmountRE.MatchString(arg.Amount) {
			// Zero or no amount given.
		} else {
			amountInt64, err := stellarnet.ParseStellarAmount(arg.Amount)
			if err != nil || amountInt64 <= 0 {
				res.amountErrMsg = "Invalid amount."
				return res
			}
			res.amountOfAsset = arg.Amount
			res.haveAmount = true
			useAmount = arg.Amount
		}
		if !res.asset.IsNativeXLM() {
			res.sendingIntentionXLM = false
			// If sending non-XLM asset, don't try to show a worth.
			return res
		}
		// Attempt to show the converted amount in outside currency.
		// Unlike when sending based on outside currency, conversion is not critical.
		if arg.From == nil {
			log("missing from address so can't convert XLM amount")
			return res
		}
		currency, err := bpc.GetOutsideCurrencyPreference(mctx, *arg.From)
		if err != nil {
			log("error getting preferred currency for %v: %v", *arg.From, err)
			return res
		}
		xrate, err := bpc.GetOutsideExchangeRate(mctx, currency)
		if err != nil {
			log("error getting exchange rate for %v: %v", currency, err)
			return res
		}
		res.rate = &xrate
		outsideAmount, err := stellarnet.ConvertXLMToOutside(useAmount, xrate.Rate)
		if err != nil {
			log("error converting: %v", err)
			return res
		}
		outsideAmountFormatted, err := FormatCurrencyWithCodeSuffix(mctx.Ctx(), mctx.G(),
			outsideAmount, xrate.Currency, FmtRound)
		if err != nil {
			log("error formatting converted outside amount: %v", err)
			return res
		}
		res.worthDescription = outsideAmountFormatted
		res.worthCurrency = string(currency)
		res.worthInfo, err = buildPaymentWorthInfo(mctx, xrate)
		if err != nil {
			log("error making worth info: %v", err)
			res.worthInfo = ""
		}

		res.displayAmountXLM, err = FormatAmountDescriptionXLM(arg.Amount)
		if err != nil {
			log("error formatting xlm %q: %s", arg.Amount, err)
			res.displayAmountXLM = ""
		}
		if arg.Amount != "" {
			res.displayAmountFiat, err = FormatCurrencyWithCodeSuffix(mctx.Ctx(), mctx.G(),
				outsideAmount, xrate.Currency, FmtRound)
			if err != nil {
				log("error formatting fiat %q / %v: %s", outsideAmount, xrate.Currency, err)
				res.displayAmountFiat = ""
			}
		}

		return res
	default:
		// This is an API contract problem.
		mctx.CWarningf("Only one of Asset and Currency parameters should be filled")
		res.amountErrMsg = "Error in communication"
		return res
	}
}

func buildPaymentWorthInfo(mctx libkb.MetaContext, rate stellar1.OutsideExchangeRate) (worthInfo string, err error) {
	oneOutsideFormatted, err := FormatCurrency(mctx.Ctx(), mctx.G(), "1", rate.Currency, FmtRound)
	if err != nil {
		return "", err
	}
	amountXLM, err := stellarnet.ConvertOutsideToXLM("1", rate.Rate)
	if err != nil {
		return "", err
	}
	amountXLMFormatted, err := FormatAmountDescriptionXLM(amountXLM)
	if err != nil {
		return "", err
	}
	worthInfo = fmt.Sprintf("%s = %s\nSource: coinmarketcap.com", oneOutsideFormatted, amountXLMFormatted)
	return worthInfo, nil
}

// Subtract a 100 stroop fee from the available balance.
// This shows the real available balance assuming an intent to send a 1 op tx.
// Does not error out, just shows the inaccurate answer.
func SubtractFeeSoft(mctx libkb.MetaContext, availableStr string) string {
	available, err := stellarnet.ParseStellarAmount(availableStr)
	if err != nil {
		mctx.CDebugf("error parsing available balance: %v", err)
		return availableStr
	}
	available -= 100
	if available < 0 {
		available = 0
	}
	return stellarnet.StringFromStellarAmount(available)
}

// Record of an in-progress payment build.
type buildPaymentEntry struct {
	Bid     stellar1.BuildPaymentID
	Stopped bool
	// The processs in Slot likely holds DataLock and pointer to Data.
	Slot     *slotctx.PrioritySlot // Only one build or review call at a time.
	DataLock sync.Mutex
	Data     buildPaymentData
}

type buildPaymentData struct {
	ReadyToReview bool
	ReadyToSend   bool
	Frozen        *frozenPayment // Latest form values.
}

type frozenPayment struct {
	From          stellar1.AccountID
	To            string
	ToIsAccountID bool
	Amount        string
	Asset         stellar1.Asset
	SecretNote    string
	PublicMemo    string
}

func newBuildPaymentEntry(bid stellar1.BuildPaymentID) *buildPaymentEntry {
	return &buildPaymentEntry{
		Bid:  bid,
		Slot: slotctx.NewPriority(),
		Data: buildPaymentData{
			ReadyToReview: false,
			ReadyToSend:   false,
		},
	}
}

// Ready decides whether the frozen payment has been prechecked and
// the Send request matches it.
func (b *buildPaymentData) CheckReadyToSend(arg stellar1.SendPaymentLocalArg) error {
	if !b.ReadyToSend {
		if !b.ReadyToReview {
			// Payment is not even ready for review.
			return fmt.Errorf("this payment is not ready to send")
		}
		// Payment is ready to review but has not been reviewed.
		return fmt.Errorf("this payment has not been reviewed")
	}
	if b.Frozen == nil {
		return fmt.Errorf("payment is ready to send but missing frozen values")
	}
	if !arg.From.Eq(b.Frozen.From) {
		return fmt.Errorf("mismatched from account: %v != %v", arg.From, b.Frozen.From)
	}
	if arg.To != b.Frozen.To {
		return fmt.Errorf("mismatched recipient: %v != %v", arg.To, b.Frozen.To)
	}
	if arg.ToIsAccountID != b.Frozen.ToIsAccountID {
		return fmt.Errorf("mismatches account ID type (expected %v)", b.Frozen.ToIsAccountID)
	}
	// Check the true amount and asset that will be sent.
	// Don't bother checking the display worth. It's finicky and the server does a coarse check.
	if arg.Amount != b.Frozen.Amount {
		return fmt.Errorf("mismatched amount: %v != %v", arg.Amount, b.Frozen.Amount)
	}
	if !arg.Asset.Eq(b.Frozen.Asset) {
		return fmt.Errorf("mismatched asset: %v != %v", arg.Asset, b.Frozen.Asset)
	}
	if arg.SecretNote != b.Frozen.SecretNote {
		// Don't log the secret memo.
		return fmt.Errorf("mismatched secret note")
	}
	if arg.PublicMemo != b.Frozen.PublicMemo {
		return fmt.Errorf("mismatched public memo: '%v' != '%v'", arg.PublicMemo, b.Frozen.PublicMemo)
	}
	return nil
}