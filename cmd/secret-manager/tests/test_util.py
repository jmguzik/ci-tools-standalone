"""Test helpers shared by the secret-manager commands."""

import sys
import unittest
from pathlib import Path

from google.api_core.exceptions import NotFound, PermissionDenied, RetryError, ServiceUnavailable
from google.auth.exceptions import DefaultCredentialsError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import util

# EXPIRED_LOGIN is the text Google reports for an expired or revoked login.
EXPIRED_LOGIN = "Getting metadata from plugin failed with error: ('invalid_grant: Token has been expired or revoked.')"


class UtilTest(unittest.TestCase):
    """Exercise util helpers without Google credentials."""

    def test_is_login_error(self):
        for name, error, want in [
            ("missing credentials", DefaultCredentialsError("none"), True),
            ("expired or revoked login", ServiceUnavailable(EXPIRED_LOGIN), True),
            ("Google outage", ServiceUnavailable("backend unavailable"), False),
            ("retry timeout after an expired login", RetryError("timeout", ServiceUnavailable(EXPIRED_LOGIN)), True),
            ("retry timeout after a Google outage", RetryError("timeout", ServiceUnavailable("backend unavailable")), False),
            ("retry timeout after another error", RetryError("timeout", NotFound("missing")), False),
            ("missing permission", PermissionDenied("denied"), False),
            ("unrelated error", ValueError("bad"), False),
        ]:
            with self.subTest(name=name):
                self.assertEqual(util.is_login_error(error), want)


if __name__ == "__main__":
    unittest.main()
