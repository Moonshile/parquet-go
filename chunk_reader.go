package go_parquet

import (
	"bytes"
	"fmt"
	"io"
	"math/bits"
	"strings"

	"github.com/pkg/errors"

	"github.com/fraugster/parquet-go/parquet"
)

var (
	errEndOfChunk = errors.New("End Of Chunk")
)

// columnChunkReader allows to read data from a single column chunk of a parquet
// file.
// TODO: get rid of this type, Reader should return an Page
type columnChunkReader struct {
	col Column

	reader    *offsetReader
	meta      *parquet.FileMetaData
	chunkMeta *parquet.ColumnMetaData

	// Definition and repetition decoder
	rDecoder, dDecoder func() levelDecoder

	dictPage *dictionaryPage

	activePage pageReader
}

type getValueDecoderFn func(parquet.Encoding) (valuesDecoder, error)

func getDictValuesEncoder(typ parquet.Type, typeLen *int32) (valuesDecoder, error) {
	switch typ {
	case parquet.Type_BYTE_ARRAY:
		return &byteArrayPlainDecoder{}, nil
	case parquet.Type_FIXED_LEN_BYTE_ARRAY:
		if typeLen == nil {
			return nil, errors.Errorf("type %s with nil type len", typ)
		}

		return &byteArrayPlainDecoder{length: int(*typeLen)}, nil
	case parquet.Type_FLOAT:
		return &floatPlainDecoder{}, nil
	case parquet.Type_DOUBLE:
		return &doublePlainDecoder{}, nil
	case parquet.Type_INT32:
		return &int32PlainDecoder{}, nil
	case parquet.Type_INT64:
		return &int64PlainDecoder{}, nil
	case parquet.Type_INT96:
		return &int96PlainDecoder{}, nil
	}

	return nil, errors.Errorf("type %s is not supported for dict value encoder", typ)
}

func getValuesDecoder(pageEncoding parquet.Encoding, typ *parquet.SchemaElement, dictValues []interface{}) (valuesDecoder, error) {
	// Change the deprecated value
	if pageEncoding == parquet.Encoding_PLAIN_DICTIONARY {
		pageEncoding = parquet.Encoding_RLE_DICTIONARY
	}

	switch *typ.Type {
	case parquet.Type_BOOLEAN:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			return &booleanPlainDecoder{}, nil
		case parquet.Encoding_RLE:
			return &booleanRLEDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	case parquet.Type_BYTE_ARRAY:
		var ret bytesArrayDecoder
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			ret = &byteArrayPlainDecoder{}
		case parquet.Encoding_DELTA_LENGTH_BYTE_ARRAY:
			ret = &byteArrayDeltaLengthDecoder{}
		case parquet.Encoding_DELTA_BYTE_ARRAY:
			ret = &byteArrayDeltaDecoder{}
		case parquet.Encoding_RLE_DICTIONARY:
			ret = &dictDecoder{values: dictValues}
		}

		if ret == nil {
			break
		}

		// Should convert to string?
		if *typ.ConvertedType == parquet.ConvertedType_UTF8 {
			return &stringDecoder{bytesArrayDecoder: ret}, nil
		}

		if typ.LogicalType != nil && typ.LogicalType.STRING != nil {
			return &stringDecoder{bytesArrayDecoder: ret}, nil
		}

		return ret, nil

	case parquet.Type_FIXED_LEN_BYTE_ARRAY:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			if typ.TypeLength == nil {
				return nil, errors.Errorf("type %s with nil type len", typ.Type)
			}

			return &byteArrayPlainDecoder{length: int(*typ.TypeLength)}, nil
		case parquet.Encoding_DELTA_BYTE_ARRAY:
			return &byteArrayDeltaDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	case parquet.Type_FLOAT:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			return &floatPlainDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	case parquet.Type_DOUBLE:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			return &doublePlainDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	case parquet.Type_INT32:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			return &int32PlainDecoder{}, nil
		case parquet.Encoding_DELTA_BINARY_PACKED:
			return &int32DeltaBPDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	case parquet.Type_INT64:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			return &int64PlainDecoder{}, nil
		case parquet.Encoding_DELTA_BINARY_PACKED:
			return &int64DeltaBPDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	case parquet.Type_INT96:
		switch pageEncoding {
		case parquet.Encoding_PLAIN:
			return &int96PlainDecoder{}, nil
		case parquet.Encoding_RLE_DICTIONARY:
			return &dictDecoder{values: dictValues}, nil
		}

	default:
		return nil, errors.Errorf("unsupported type: %s", typ.Type)
	}

	return nil, errors.Errorf("unsupported encoding %s for %s type", pageEncoding, typ.Type)
}

func newColumnChunkReader(r io.ReadSeeker, meta *parquet.FileMetaData, col Column, chunk *parquet.ColumnChunk) (*columnChunkReader, error) {
	if chunk.FilePath != nil {
		return nil, fmt.Errorf("nyi: data is in another file: '%s'", *chunk.FilePath)
	}

	c := col.Index()
	// chunk.FileOffset is useless so ChunkMetaData is required here
	// as we cannot read it from r
	// see https://issues.apache.org/jira/browse/PARQUET-291
	if chunk.MetaData == nil {
		return nil, errors.Errorf("missing meta data for column %c", c)
	}

	if typ := *col.Element().Type; chunk.MetaData.Type != typ {
		return nil, errors.Errorf("wrong type in column chunk metadata, expected %s was %s",
			typ, chunk.MetaData.Type)
	}

	offset := chunk.MetaData.DataPageOffset
	if chunk.MetaData.DictionaryPageOffset != nil {
		offset = *chunk.MetaData.DictionaryPageOffset
	}
	// Seek to the beginning of the first Page
	_, err := r.Seek(offset, io.SeekStart)
	if err != nil {
		return nil, err
	}

	cr := &columnChunkReader{
		col: col,
		reader: &offsetReader{
			inner:  r,
			offset: offset,
			count:  0,
		},
		meta:      meta,
		chunkMeta: chunk.MetaData,
	}
	nested := strings.IndexByte(col.FlatName(), '.') >= 0
	repType := *col.Element().RepetitionType
	if !nested && repType == parquet.FieldRepetitionType_REQUIRED {
		// TODO: also check that len(Path) = maxD
		// For data that is required, the definition levels are not encoded and
		// always have the value of the max definition level.
		// TODO: document level ranges
		cr.dDecoder = func() levelDecoder {
			return &levelDecoderWrapper{decoder: constDecoder(int32(col.MaxDefinitionLevel())), max: cr.col.MaxDefinitionLevel()}
		}
	} else {
		cr.dDecoder = func() levelDecoder {
			dec := newHybridDecoder(bits.Len16(col.MaxDefinitionLevel()))
			dec.buffered = true
			return &levelDecoderWrapper{decoder: dec, max: cr.col.MaxDefinitionLevel()}
		}
	}
	if !nested && repType != parquet.FieldRepetitionType_REPEATED {
		// TODO: I think we need to check all schemaElements in the path (confirm if above)
		// TODO: clarify the following comment from parquet-format/README:
		// If the column is not nested the repetition levels are not encoded and
		// always have the value of 1
		cr.rDecoder = func() levelDecoder {
			return &levelDecoderWrapper{decoder: constDecoder(0), max: cr.col.MaxRepetitionLevel()}
		}
	} else {
		cr.rDecoder = func() levelDecoder {
			dec := newHybridDecoder(bits.Len16(col.MaxRepetitionLevel()))
			dec.buffered = true
			return &levelDecoderWrapper{decoder: dec, max: cr.col.MaxRepetitionLevel()}
		}
	}

	return cr, nil
}

func createDataReader(r io.Reader, codec parquet.CompressionCodec, compressedSize int32, uncompressedSize int32) (io.Reader, error) {
	if compressedSize < 0 || uncompressedSize < 0 {
		return nil, errors.New("invalid page data size")
	}

	return newBlockReader(r, codec, compressedSize, uncompressedSize)
}

// dictionaryPage is not a real data page, so there is no need to implement the page interface
type dictionaryPage struct {
	ph *parquet.PageHeader

	numValues int32
	enc       valuesDecoder

	values []interface{}
}

func (dp *dictionaryPage) init(dict valuesDecoder) error {
	if dict == nil {
		return errors.New("dictionary page without dictionary value encoder")
	}

	dp.enc = dict
	return nil
}

func (dp *dictionaryPage) read(r io.ReadSeeker, ph *parquet.PageHeader, codec parquet.CompressionCodec) error {
	if ph.DictionaryPageHeader == nil {
		return errors.Errorf("null DictionaryPageHeader in %+v", ph)
	}

	if dp.numValues = ph.DictionaryPageHeader.NumValues; dp.numValues < 0 {
		return errors.Errorf("negative NumValues in DICTIONARY_PAGE: %d", dp.numValues)
	}

	if ph.DictionaryPageHeader.Encoding != parquet.Encoding_PLAIN && ph.DictionaryPageHeader.Encoding != parquet.Encoding_PLAIN_DICTIONARY {
		return errors.Errorf("only Encoding_PLAIN and Encoding_PLAIN_DICTIONARY is supported for dict values encoder")
	}

	dp.ph = ph

	reader, err := createDataReader(r, codec, ph.GetCompressedPageSize(), ph.GetUncompressedPageSize())
	if err != nil {
		return err
	}

	dp.values = make([]interface{}, dp.numValues)
	if err := dp.enc.init(reader); err != nil {
		return err
	}

	// no error is accepted here, even EOF
	if n, err := dp.enc.decodeValues(dp.values); err != nil {
		return errors.Wrapf(err, "expected %d value read %d value", dp.numValues, n)
	}

	return nil
}

type dataPageV1 struct {
	ph *parquet.PageHeader

	numValues          int32
	encoding           parquet.Encoding
	dDecoder, rDecoder levelDecoder
	valuesDecoder      valuesDecoder
	fn                 getValueDecoderFn

	position int
}

func (dp *dataPageV1) readValues(val []interface{}) (n int, dLevel []uint16, rLevel []uint16, err error) {
	size := len(val)
	if rem := int(dp.numValues) - dp.position; rem < size {
		size = rem
	}

	if size == 0 {
		return 0, nil, nil, nil
	}

	dLevel = make([]uint16, size)
	if err := decodeUint16(dp.dDecoder, dLevel); err != nil {
		return 0, nil, nil, errors.Wrap(err, "read definition levels failed")
	}

	rLevel = make([]uint16, size)
	if err := decodeUint16(dp.rDecoder, rLevel); err != nil {
		return 0, nil, nil, errors.Wrap(err, "read repetition levels failed")
	}

	notNull := 0
	for _, dl := range dLevel {
		if dl == dp.dDecoder.maxLevel() {
			notNull++
		}
	}

	if notNull != 0 {
		if n, err := dp.valuesDecoder.decodeValues(val[:notNull]); err != nil {
			return 0, nil, nil, errors.Wrapf(err, "read values from page failed, need %d value read %d", notNull, n)
		}
	}
	dp.position += size
	return size, dLevel, rLevel, nil
}

func (dp *dataPageV1) init(dDecoder, rDecoder func() levelDecoder, values getValueDecoderFn) error {
	dp.dDecoder = dDecoder()
	dp.rDecoder = rDecoder()
	dp.fn = values
	dp.position = 0

	return nil
}

func (dp *dataPageV1) read(r io.ReadSeeker, ph *parquet.PageHeader, codec parquet.CompressionCodec) (err error) {
	if ph.DataPageHeader == nil {
		return errors.Errorf("null DataPageHeader in %+v", ph)
	}

	if dp.numValues = ph.DataPageHeader.NumValues; dp.numValues < 0 {
		return errors.Errorf("negative NumValues in DATA_PAGE: %d", dp.numValues)
	}
	dp.encoding = ph.DataPageHeader.Encoding
	dp.ph = ph

	if dp.valuesDecoder, err = dp.fn(dp.encoding); err != nil {
		return err
	}

	if err := dp.dDecoder.initSize(r); err != nil {
		return err
	}

	if err := dp.rDecoder.initSize(r); err != nil {
		return err
	}

	reader, err := createDataReader(r, codec, ph.GetCompressedPageSize(), ph.GetUncompressedPageSize())
	if err != nil {
		return err
	}

	return dp.valuesDecoder.init(reader)
}

type dataPageV2 struct {
	ph *parquet.PageHeader

	numValues          int32
	encoding           parquet.Encoding
	valuesDecoder      valuesDecoder
	dDecoder, rDecoder levelDecoder
	fn                 getValueDecoderFn
	position           int
}

func (dp *dataPageV2) readValues(val []interface{}) (n int, dLevel []uint16, rLevel []uint16, err error) {
	size := len(val)
	if rem := int(dp.numValues) - dp.position; rem < size {
		size = rem
	}

	if size == 0 {
		return 0, nil, nil, nil
	}

	dLevel = make([]uint16, size)
	if err := decodeUint16(dp.dDecoder, dLevel); err != nil {
		return 0, nil, nil, errors.Wrap(err, "read definition levels failed")
	}

	rLevel = make([]uint16, size)
	if err := decodeUint16(dp.rDecoder, rLevel); err != nil {
		return 0, nil, nil, errors.Wrap(err, "read repetition levels failed")
	}

	notNull := 0
	for _, dl := range dLevel {
		if dl == dp.dDecoder.maxLevel() {
			notNull++
		}
	}

	if notNull != 0 {
		if n, err := dp.valuesDecoder.decodeValues(val[:notNull]); err != nil {
			return 0, nil, nil, errors.Wrapf(err, "read values from page failed, need %d values but read %d", notNull, n)
		}
	}
	dp.position += size
	return size, dLevel, rLevel, nil
}

func (dp *dataPageV2) init(dDecoder, rDecoder func() levelDecoder, values getValueDecoderFn) error {
	dp.dDecoder = dDecoder()
	dp.rDecoder = rDecoder()
	dp.fn = values
	dp.position = 0

	return nil
}

func (dp *dataPageV2) read(r io.ReadSeeker, ph *parquet.PageHeader, codec parquet.CompressionCodec) (err error) {
	// TODO: verify this format, there is some question
	// 1- Uncompressed size is affected by the level lens?
	// 2- If the levels are actually rle and the first byte is the size, since there is already size in header (NO)
	if ph.DataPageHeaderV2 == nil {
		return errors.Errorf("null DataPageHeaderV2 in %+v", ph)
	}

	if dp.numValues = ph.DataPageHeaderV2.NumValues; dp.numValues < 0 {
		return errors.Errorf("negative NumValues in DATA_PAGE_V2: %d", dp.numValues)
	}

	if ph.DataPageHeaderV2.RepetitionLevelsByteLength < 0 {
		return errors.Errorf("invalid RepetitionLevelsByteLength")
	}
	if ph.DataPageHeaderV2.DefinitionLevelsByteLength < 0 {
		return errors.Errorf("invalid DefinitionLevelsByteLength")
	}
	dp.encoding = ph.DataPageHeader.Encoding
	dp.ph = ph

	if dp.valuesDecoder, err = dp.fn(dp.encoding); err != nil {
		return err
	}

	// Its safe to call this {r,d}Decoder later, since the stream they operate on are in memory
	levelsSize := ph.DataPageHeaderV2.RepetitionLevelsByteLength + ph.DataPageHeaderV2.DefinitionLevelsByteLength
	// read both level size
	if levelsSize > 0 {
		data := make([]byte, levelsSize)
		n, err := io.ReadFull(r, data)
		if err != nil {
			return errors.Wrapf(err, "need to read %d byte but there was only %d byte", levelsSize, n)
		}
		if ph.DataPageHeaderV2.RepetitionLevelsByteLength > 0 {
			if err := dp.rDecoder.init(bytes.NewReader(data[:int(ph.DataPageHeaderV2.RepetitionLevelsByteLength)])); err != nil {
				return errors.Wrapf(err, "read repetition level failed")
			}
		}
		if ph.DataPageHeaderV2.DefinitionLevelsByteLength > 0 {
			if err := dp.dDecoder.init(bytes.NewReader(data[int(ph.DataPageHeaderV2.RepetitionLevelsByteLength):])); err != nil {
				return errors.Wrapf(err, "read definition level failed")
			}
		}
	}

	// TODO: (F0rud) I am not sure if this is correct to subtract the level size from the compressed size here
	reader, err := createDataReader(r, codec, ph.GetCompressedPageSize()-levelsSize, ph.GetUncompressedPageSize()-levelsSize)
	if err != nil {
		return err
	}

	return dp.valuesDecoder.init(reader)
}

func (cr *columnChunkReader) readPage() (pageReader, error) {
	if cr.chunkMeta.TotalCompressedSize-cr.reader.Count() <= 0 {
		return nil, errEndOfChunk
	}
	ph := &parquet.PageHeader{}
	if err := readThrift(ph, cr.reader); err != nil {
		return nil, err
	}

	if ph.Type == parquet.PageType_DICTIONARY_PAGE {
		if cr.dictPage != nil {
			return nil, errors.New("there should be only one dictionary")
		}
		p := &dictionaryPage{}
		de, err := getDictValuesEncoder(*cr.col.Element().Type, cr.col.Element().TypeLength)
		if err != nil {
			return nil, err
		}
		if err := p.init(de); err != nil {
			return nil, err
		}

		if err := p.read(cr.reader, ph, cr.chunkMeta.Codec); err != nil {
			return nil, err
		}

		cr.dictPage = p
		// Go to the next data Page
		// if we have a DictionaryPageOffset we should return to DataPageOffset
		if cr.chunkMeta.DictionaryPageOffset != nil {
			if *cr.chunkMeta.DictionaryPageOffset != cr.reader.offset {
				if _, err := cr.reader.Seek(cr.chunkMeta.DataPageOffset, io.SeekStart); err != nil {
					return nil, err
				}
			}
		}

		// Return the real data Page, not the dictionary Page
		return cr.readPage()
	}

	var p pageReader
	switch ph.Type {
	case parquet.PageType_DATA_PAGE:
		p = &dataPageV1{}
	case parquet.PageType_DATA_PAGE_V2:
		p = &dataPageV2{}
	default:
		return nil, errors.Errorf("DATA_PAGE or DATA_PAGE_V2 type expected, but was %s", ph.Type)
	}
	var dictValue []interface{}
	if cr.dictPage != nil {
		dictValue = cr.dictPage.values
	}
	var fn = func(typ parquet.Encoding) (valuesDecoder, error) {
		return getValuesDecoder(typ, cr.col.Element(), dictValue)
	}
	if err := p.init(cr.dDecoder, cr.rDecoder, fn); err != nil {
		return nil, err
	}

	if err := p.read(cr.reader, ph, cr.chunkMeta.Codec); err != nil {
		return nil, err
	}

	return p, nil
}

func (cr *columnChunkReader) Read(values []interface{}) (n int, dLevel []uint16, rLevel []uint16, err error) {
	// TODO : the docs said each chunk could have one or more page, one is ok, two is ok since dictionary pages are always in pair
	// but is there any chunk with 3 page or above? if there is, this function needs re-write.
	if cr.activePage == nil {
		cr.activePage, err = cr.readPage()
		if err == errEndOfChunk { // if this is the end of chunk
			return n, dLevel, rLevel, nil
		}

		if err != nil {
			return 0, nil, nil, errors.Wrap(err, "read page failed")
		}
	}

	return cr.activePage.readValues(values[n:])
}