// SPDX-License-Identifier: Apache-2.0
package setup

import "os"

func smiCandidates() []string { return winSMICandidates(os.Getenv) }
