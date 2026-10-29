"""Test helpers shared by the secret-manager commands."""

import sys
import unittest
from pathlib import Path

from google.api_core.exceptions import NotFound, PermissionDenied, RetryError, ServiceUnavailable
from google.auth.exceptions import DefaultCredentialsError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import util


class UtilTest(unittest.TestCase):
    """Exercise util helpers without Google credentials."""

    def test_is_login_error(self):
        for name, error, want in [
            ("missing credentials", DefaultCredentialsError("none"), True),
            ("expired or revoked login", ServiceUnavailable("invalid_grant"), True),
            ("retry timeout after an expired login", RetryError("timeout", ServiceUnavailable("invalid_grant")), True),
            ("retry timeout after another error", RetryError("timeout", NotFound("missing")), False),
            ("missing permission", PermissionDenied("denied"), False),
            ("unrelated error", ValueError("bad"), False),
        ]:
            with self.subTest(name=name):
                self.assertEqual(util.is_login_error(error), want)


if __name__ == "__main__":
    unittest.main()
