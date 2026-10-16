from app.orders import short


def test_short():
    assert short(1) == 2
