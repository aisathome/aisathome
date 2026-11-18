"""Fixtures for the browser tests: one headless Chromium for the session (HEADED=1 shows it).

SPDX-License-Identifier: Apache-2.0
"""
import os

import pytest
from playwright.sync_api import sync_playwright


@pytest.fixture(scope="session")
def browser():
    with sync_playwright() as p:
        b = p.chromium.launch(headless=os.environ.get("HEADED") != "1")
        yield b
        b.close()
