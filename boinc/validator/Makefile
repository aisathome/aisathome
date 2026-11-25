# SPDX-License-Identifier: Apache-2.0
#
# The pilot app's validator, built against a BOINC server source tree whose lib/, db/ and
# sched/ libraries are built (the tree the project's server comes from, at the same release), and
# its svn_version.h generated (./generate_svn_version.sh at its top):
#
#   make BOINC=/path/to/boinc            # pilot_validator
#   make BOINC=/path/to/boinc test       # the fixture test of check_set and check_pair
#
# Rebuild it with every server upgrade.

BOINC ?=
CXX ?= g++
CXXFLAGS ?= -O2 -g -Wall
MYSQL_CONFIG ?= mysql_config

INC := -I$(BOINC) -I$(BOINC)/lib -I$(BOINC)/db -I$(BOINC)/sched $(shell $(MYSQL_CONFIG) --include)
LIBS := $(BOINC)/sched/.libs/libsched.a $(BOINC)/lib/.libs/libboinc_crypt.a $(BOINC)/lib/.libs/libboinc.a \
	$(shell $(MYSQL_CONFIG) --libs) -lssl -lcrypto -lz -lpthread
# BOINC's validator framework (VALIDATOR_SOURCES in sched/Makefile.am) without validate_util2.cpp:
# pilot_validator.cpp supplies check_set and check_pair itself.
FRAMEWORK := $(BOINC)/sched/credit.cpp $(BOINC)/sched/validator.cpp $(BOINC)/sched/validate_util.cpp

all: pilot_validator

pilot_validator: pilot_validator.cpp | boinc-tree
	$(CXX) $(CXXFLAGS) $(INC) -o $@ pilot_validator.cpp $(FRAMEWORK) $(LIBS)

check_set_test: check_set_test.cpp pilot_validator.cpp | boinc-tree
	$(CXX) $(CXXFLAGS) $(INC) -o $@ check_set_test.cpp pilot_validator.cpp $(LIBS)

test: pilot_validator check_set_test
	./check_set_test

boinc-tree:
	@test -n "$(BOINC)" -a -f "$(BOINC)/sched/.libs/libsched.a" -a -f "$(BOINC)/svn_version.h" || \
		{ echo "BOINC=<a BOINC source tree with lib/, db/ and sched/ built and svn_version.h> is needed"; exit 1; }

clean:
	rm -f pilot_validator check_set_test

.PHONY: all test clean boinc-tree
