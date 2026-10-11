/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rules_test

import (
	"testing"
	"time"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
)

// upgradeRuleOf returns the NV-UPG-001 rule out of a rule set, so that a test
// can run the one rule the options reach.
func upgradeRuleOf(t *testing.T, set []health.Rule) health.Rule {
	t.Helper()

	for _, rule := range set {
		if rule.Code() == "NV-UPG-001" {
			return rule
		}
	}

	t.Fatal("the rule set holds no NV-UPG-001")

	return nil
}

// The window in Options has to be the window the rule uses, or the flag is a
// flag that parses and does nothing.
func TestAllWithCarriesTheUpgradeWindowToTheRule(t *testing.T) {
	t.Parallel()

	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-45*time.Minute)))

	wide := upgradeRuleOf(t, rules.AllWith(rules.Options{UpgradeStuckAfter: 2 * time.Hour}))
	if findings := wide.Evaluate(in); len(findings) != 0 {
		t.Fatalf("at a 2h window the node is not stuck: got %d findings: %+v", len(findings), findings)
	}

	narrow := upgradeRuleOf(t, rules.AllWith(rules.Options{UpgradeStuckAfter: time.Minute}))
	if findings := narrow.Evaluate(in); len(findings) != 1 {
		t.Fatalf("at a 1m window the node is stuck: got %d findings, want 1", len(findings))
	}
}

// A zero Options is the defaults, and All is that call. Forty-five minutes is
// past the thirty minute default, so the rule has to fire.
func TestAllUsesTheDefaultUpgradeWindow(t *testing.T) {
	t.Parallel()

	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-45*time.Minute)))

	finding := only(t, upgradeRuleOf(t, rules.All()).Evaluate(in))
	if got := finding.Evidence["upgradeStuckAfter"]; got != rules.DefaultUpgradeStuckAfter.String() {
		t.Errorf("upgradeStuckAfter is %q, want the default %q", got, rules.DefaultUpgradeStuckAfter)
	}
}

// All and AllWith have to be the same set, or a rule could be reachable
// through one and not the other.
func TestAllAndAllWithHoldTheSameCodes(t *testing.T) {
	t.Parallel()

	withOptions := rules.AllWith(rules.Options{UpgradeStuckAfter: time.Hour})
	plain := rules.All()

	if len(plain) != len(withOptions) {
		t.Fatalf("All returns %d rules and AllWith returns %d", len(plain), len(withOptions))
	}

	for i := range plain {
		if plain[i].Code() != withOptions[i].Code() {
			t.Errorf("rule %d is %s through All and %s through AllWith", i, plain[i].Code(), withOptions[i].Code())
		}
	}
}
