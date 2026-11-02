package classify

// DefaultRules are examples to edit for your estate (PUT /v1/classification/rules).
// They key on destinations whose data class is obvious from the service.
func DefaultRules() RuleSet {
	return RuleSet{Rules: []Rule{
		{ID: "india-payment-gateways", Enabled: true, Description: "Indian payment processors and UPI endpoints carry RBI-regulated payment data.",
			Match: Match{Hosts: []string{"*.razorpay.com", "*.paytm.com", "*.npci.org.in", "*.billdesk.com", "*.payu.in", "*.cashfree.com"}}, Classifications: []string{"payment_system_data"}},
		{ID: "india-payment-apps", Enabled: true, Description: "UPI and card-present apps run by the Indian business unit (RBI payment data localisation).",
			Match: Match{Apps: []string{"upi-payments", "pos-terminal"}}, Classifications: []string{"payment_system_data"}},
		{ID: "eu-health-records", Enabled: true, Description: "EU electronic health record services (special-category data under GDPR Art. 9).",
			Match: Match{Apps: []string{"ehr", "patient-portal"}}, Classifications: []string{"sensitive_eu_citizen_data", "eu_special_category_data"}},
		{ID: "eu-customer-crm", Enabled: true, Description: "Customer CRM traffic of the EU business unit.",
			Match: Match{Apps: []string{"crm-eu"}}, Classifications: []string{"eu_personal_data"}},
		{ID: "california-consumer-apps", Enabled: true, Description: "Consumer apps serving California residents.",
			Match: Match{Apps: []string{"consumer-app-ca"}}, Classifications: []string{"california_consumer_data"}},
		{ID: "us-genomics-bulk", Enabled: true, Description: "Genomic data pipeline (bulk U.S. sensitive data under 28 CFR 202).",
			Match: Match{Apps: []string{"genomics-pipeline"}, Ports: []int{443}}, Classifications: []string{"us_bulk_sensitive_data"}},
	}}
}
